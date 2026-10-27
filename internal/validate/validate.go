// Package validate checks a sender's event against envelope v1: the JSON
// Schema, the generated Go types, and the rules the schema cannot express.
package validate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/schema"
)

// MaxBodyBytes is the limit on the body field, counted in bytes. The schema
// counts characters, so the server checks bytes itself.
const MaxBodyBytes = 256 * 1024

// ServerFields are the readOnly fields a sender may not set.
var ServerFields = []string{"id", "received_at", "hold", "delivery"}

// Error is a validation failure, written for a person to read.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// IsValidation reports whether err is a validation failure (a 400), as
// opposed to a server fault.
func IsValidation(err error) bool {
	var v *Error
	return errors.As(err, &v)
}

// Validator checks inputs. It is safe for concurrent use.
type Validator struct {
	input *jsonschema.Schema
}

// New compiles the embedded envelope schema.
func New() (*Validator, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.Envelope))
	if err != nil {
		return nil, fmt.Errorf("envelope schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(schema.EnvelopeID, doc); err != nil {
		return nil, err
	}
	s, err := c.Compile(schema.EnvelopeID)
	if err != nil {
		return nil, fmt.Errorf("envelope schema: %w", err)
	}
	return &Validator{input: s}, nil
}

// Input is a checked sender event.
type Input struct {
	// Fields is the event as the sender posted it, numbers kept exact.
	Fields map[string]any
	// Typed is the same event decoded into the generated type.
	Typed api.EventInput
}

// Input checks raw JSON as a sender's EventInput.
func (v *Validator) Input(raw []byte) (*Input, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return nil, fail("body is not a JSON object: %v", err)
	}
	if dec.More() {
		return nil, fail("body holds more than one JSON value")
	}
	return v.Fields(fields)
}

// Fields checks an already decoded object. Numbers should be json.Number, which
// jsonschema reads natively.
func (v *Validator) Fields(fields map[string]any) (*Input, error) {
	if fields == nil {
		return nil, fail("body is not a JSON object")
	}
	var owned []string
	for _, k := range ServerFields {
		if _, ok := fields[k]; ok {
			owned = append(owned, k)
		}
	}
	if len(owned) > 0 {
		return nil, fail("%s: set by the server, not allowed in input", strings.Join(owned, ", "))
	}
	if err := v.input.Validate(any(fields)); err != nil {
		return nil, fail("envelope: %s", oneLine(err))
	}
	if b, ok := fields["body"].(string); ok && len(b) > MaxBodyBytes {
		return nil, fail("body: %d bytes, the limit is %d", len(b), MaxBodyBytes)
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	in := &Input{Fields: fields}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in.Typed); err != nil {
		return nil, fail("envelope: %v", err)
	}
	if c := in.Typed.Content; c != nil && c.Steps.IsSpecified() && !c.Steps.IsNull() {
		s, _ := c.Steps.Get()
		if s.Current > s.Total {
			return nil, fail("content.steps: current %d exceeds total %d", s.Current, s.Total)
		}
	}
	return in, nil
}

var printer = message.NewPrinter(language.English)

// oneLine flattens a jsonschema error tree into its leaf messages.
func oneLine(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	var msgs []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := "/" + strings.Join(e.InstanceLocation, "/")
			msgs = append(msgs, fmt.Sprintf("%s: %s", loc, e.ErrorKind.LocalizedString(printer)))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	// unevaluatedProperties reports "false schema" for every field once any
	// branch fails. Keep those only when nothing else explains the failure,
	// and then say what they mean.
	var real, unknown []string
	for _, m := range msgs {
		if loc, ok := strings.CutSuffix(m, ": false schema"); ok {
			unknown = append(unknown, loc+": not allowed here")
		} else {
			real = append(real, m)
		}
	}
	msgs = real
	if len(msgs) == 0 {
		msgs = unknown
	}
	sort.Strings(msgs)
	msgs = dedupe(msgs)
	if len(msgs) > 5 {
		msgs = append(msgs[:5], fmt.Sprintf("and %d more", len(msgs)-5))
	}
	return strings.Join(msgs, "; ")
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}
