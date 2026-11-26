package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/delivery"
)

// replayDelivery runs delivery settings over an event export in memory and
// fails when they make an event louder, lower a keep-loud or protected
// event, or take an incident or keep-loud event out of the badge without a
// row that names it.
func replayDelivery(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("replay-delivery", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rowsPath := fs.String("settings", "", "rows: a JSON list of DeliverySettingInput, or a GET /v1/delivery answer; none runs the identity check alone")
	keepPath := fs.String("keep", "", "keep-loud list: a JSON list of [source, title] lists or {\"id\": ...} objects")
	scope := fs.String("scope", "attention", "badge.scope: attention, pushed or none")
	digestOn := fs.Bool("digest", false, "run as if digest.enabled were on")
	newLevel := fs.String("new-source-level", "passive", "delivery.new_source_level")
	verbose := fs.Bool("v", false, "print every event the rows change")
	usage := errors.New("usage: heraldo-server replay-delivery [-settings <rows.json>] [-keep <pairs.json>] [-scope s] [-digest] [-v] <events.jsonl>")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return usage
	}
	var rows []delivery.Row
	if *rowsPath != "" {
		raw, err := os.ReadFile(*rowsPath)
		if err != nil {
			return err
		}
		if rows, err = delivery.ParseRows(raw); err != nil {
			return fmt.Errorf("%s: %w", *rowsPath, err)
		}
	}
	var keep []delivery.Pair
	if *keepPath != "" {
		raw, err := os.ReadFile(*keepPath)
		if err != nil {
			return err
		}
		if keep, err = parseKeep(raw); err != nil {
			return fmt.Errorf("%s: %w", *keepPath, err)
		}
	}
	evs, err := readEvents(fs.Arg(0))
	if err != nil {
		return err
	}
	rep := delivery.Replay(evs, rows, keep, *scope,
		delivery.Config{NewSourceLevel: api.Level(*newLevel), DigestOn: *digestOn})
	rep.Print(stdout, *verbose)
	if len(rep.Failures) > 0 {
		return fmt.Errorf("replay-delivery: %d problems", len(rep.Failures))
	}
	return nil
}

// parseKeep reads a keep-loud list: each entry a [source, title, ...] list,
// or an object with id (one event) or source and title.
func parseKeep(raw []byte) ([]delivery.Pair, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("want a list: %w", err)
	}
	out := make([]delivery.Pair, 0, len(list))
	for i, x := range list {
		var arr []string
		if json.Unmarshal(x, &arr) == nil {
			if len(arr) < 2 {
				return nil, fmt.Errorf("entry %d: want [source, title]", i)
			}
			out = append(out, delivery.Pair{Source: arr[0], Title: arr[1]})
			continue
		}
		var obj struct{ ID, Source, Title string }
		if err := json.Unmarshal(x, &obj); err != nil || (obj.ID == "" && obj.Source == "") {
			return nil, fmt.Errorf("entry %d: want [source, title], {\"id\": ...} or {\"source\": ..., \"title\": ...}", i)
		}
		out = append(out, delivery.Pair{ID: obj.ID, Source: obj.Source, Title: obj.Title})
	}
	return out, nil
}

// readEvents reads stored events, one JSON object per line.
func readEvents(path string) ([]api.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []api.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev api.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}
