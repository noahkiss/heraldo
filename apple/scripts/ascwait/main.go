// Command ascwait waits until App Store Connect has processed one uploaded
// build, and reports whether Apple accepted it. testflight.sh runs it after
// the upload.
//
//	ascwait -key AuthKey.p8 -key-id ID -issuer ISSUER -bundle-id ID -build N
//
// Exit status: 0 processed, 1 rejected (Apple's errors on stderr), 2 state
// unknown (timeout, refused credentials, bad flags).
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	exitComplete = 0
	exitFailed   = 1
	exitUnknown  = 2
)

func main() {
	var (
		keyPath  = flag.String("key", "", "path to the App Store Connect API key (.p8)")
		keyID    = flag.String("key-id", "", "API key ID")
		issuer   = flag.String("issuer", "", "issuer ID")
		bundleID = flag.String("bundle-id", "", "the app's bundle ID")
		build    = flag.String("build", "", "build number (CFBundleVersion) to wait for")
		platform = flag.String("platform", "IOS", "IOS, MAC_OS, TV_OS or VISION_OS")
		timeout  = flag.Duration("timeout", 30*time.Minute, "give up after this long")
		interval = flag.Duration("interval", 30*time.Second, "time between checks")
	)
	flag.Parse()
	if *keyPath == "" || *keyID == "" || *issuer == "" || *bundleID == "" || *build == "" {
		fmt.Fprintln(os.Stderr, "ascwait: -key, -key-id, -issuer, -bundle-id and -build are required")
		os.Exit(exitUnknown)
	}
	pemBytes, err := os.ReadFile(*keyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ascwait:", err)
		os.Exit(exitUnknown)
	}
	key, err := parseKey(pemBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ascwait:", err)
		os.Exit(exitUnknown)
	}
	w := &waiter{
		api:      &client{base: "https://api.appstoreconnect.apple.com", http: &http.Client{Timeout: 30 * time.Second}, key: key, keyID: *keyID, issuer: *issuer, now: time.Now},
		bundleID: *bundleID, build: *build, platform: *platform,
		timeout: *timeout, interval: *interval,
		now: time.Now, sleep: time.Sleep, log: os.Stderr,
	}
	os.Exit(w.run(context.Background(), os.Stdout))
}

// parseKey reads the .p8 file Apple hands out: a PEM PKCS #8 P-256 key.
func parseKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("API key: no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("API key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok || ec.Curve.Params().BitSize != 256 {
		return nil, errors.New("API key: not a P-256 key")
	}
	return ec, nil
}

// tokenLifetime stays under Apple's 20 minute cap.
const tokenLifetime = 10 * time.Minute

// signToken makes the App Store Connect API token, an ES256 JWT.
func signToken(key *ecdsa.PrivateKey, keyID, issuer string, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": keyID, "typ": "JWT"})
	c, _ := json.Marshal(map[string]any{
		"iss": issuer, "iat": now.Unix(), "exp": now.Add(tokenLifetime).Unix(),
		"aud": "appstoreconnect-v1",
	})
	unsigned := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	sum := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	// JWS ES256 is r and s, 32 bytes each, big-endian; not DER.
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return unsigned + "." + enc.EncodeToString(out), nil
}

// httpError is a non-2xx answer from the API.
type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("App Store Connect answered %d: %s", e.status, e.body)
}

var errNoApp = errors.New("no app with this bundle ID in App Store Connect")

// fatal reports whether polling again cannot help: the credentials, the
// request or the bundle ID are wrong. Network errors, 5xx and 429 are worth
// another try.
func fatal(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.status >= 400 && he.status < 500 && he.status != 429
	}
	return errors.Is(err, errNoApp)
}

type client struct {
	base   string
	http   *http.Client
	key    *ecdsa.PrivateKey
	keyID  string
	issuer string
	now    func() time.Time
}

// get fetches path with a fresh token, so a long wait never outlives one.
func (c *client) get(ctx context.Context, path string, q url.Values, out any) error {
	tok, err := signToken(c.key, c.keyID, c.issuer, c.now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &httpError{resp.StatusCode, strings.TrimSpace(string(body))}
	}
	return json.Unmarshal(body, out)
}

type appsResponse struct {
	Data []struct {
		ID         string `json:"id"`
		Attributes struct {
			BundleID string `json:"bundleId"`
		} `json:"attributes"`
	} `json:"data"`
}

// detail is Apple's StateDetail.
type detail struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

type buildUpload struct {
	ID         string `json:"id"`
	Attributes struct {
		Version      string    `json:"cfBundleShortVersionString"`
		Build        string    `json:"cfBundleVersion"`
		CreatedDate  time.Time `json:"createdDate"`
		UploadedDate time.Time `json:"uploadedDate"`
		State        struct {
			State    string   `json:"state"`
			Errors   []detail `json:"errors"`
			Warnings []detail `json:"warnings"`
			Infos    []detail `json:"infos"`
		} `json:"state"`
	} `json:"attributes"`
	Relationships struct {
		Build struct {
			Data *struct {
				ID string `json:"id"`
			} `json:"data"`
		} `json:"build"`
	} `json:"relationships"`
}

type buildUploadsResponse struct {
	Data     []buildUpload `json:"data"`
	Included []struct {
		Type       string `json:"type"`
		ID         string `json:"id"`
		Attributes struct {
			ProcessingState string `json:"processingState"`
		} `json:"attributes"`
	} `json:"included"`
}

// buildState is the processingState of the build an upload made (PROCESSING,
// FAILED, INVALID or VALID), or "" when the upload has no build yet.
func (r *buildUploadsResponse) buildState(u *buildUpload) string {
	b := u.Relationships.Build.Data
	if b == nil {
		return ""
	}
	for _, inc := range r.Included {
		if inc.Type == "builds" && inc.ID == b.ID {
			return inc.Attributes.ProcessingState
		}
	}
	return ""
}

// appID finds the app's App Store Connect ID by its exact bundle ID.
func (c *client) appID(ctx context.Context, bundleID string) (string, error) {
	var r appsResponse
	q := url.Values{"filter[bundleId]": {bundleID}, "fields[apps]": {"bundleId"}}
	if err := c.get(ctx, "/v1/apps", q, &r); err != nil {
		return "", err
	}
	for _, a := range r.Data {
		if a.Attributes.BundleID == bundleID {
			return a.ID, nil
		}
	}
	return "", fmt.Errorf("%w: %s", errNoApp, bundleID)
}

// upload returns the newest upload of build for the platform, or nil when
// App Store Connect does not list it yet, and its build's processingState.
func (c *client) upload(ctx context.Context, appID, build, platform string) (*buildUpload, string, error) {
	var r buildUploadsResponse
	q := url.Values{
		"filter[cfBundleVersion]": {build}, "filter[platform]": {platform},
		"include": {"build"}, "fields[builds]": {"processingState"},
	}
	if err := c.get(ctx, "/v1/apps/"+url.PathEscape(appID)+"/buildUploads", q, &r); err != nil {
		return nil, "", err
	}
	u := newest(r.Data, build)
	if u == nil {
		return nil, "", nil
	}
	return u, r.buildState(u), nil
}

// newest picks the latest upload of build; a retried upload reuses the number.
func newest(ups []buildUpload, build string) *buildUpload {
	var best *buildUpload
	for i := range ups {
		u := &ups[i]
		if u.Attributes.Build != build {
			continue
		}
		if best == nil || stamp(u).After(stamp(best)) {
			best = u
		}
	}
	return best
}

func stamp(u *buildUpload) time.Time {
	if !u.Attributes.UploadedDate.IsZero() {
		return u.Attributes.UploadedDate
	}
	return u.Attributes.CreatedDate
}

type waiter struct {
	api                       *client
	bundleID, build, platform string
	timeout, interval         time.Duration
	now                       func() time.Time
	sleep                     func(time.Duration)
	log                       io.Writer
}

// run polls until the upload is COMPLETE or FAILED, or the timeout passes.
// It checks once more after the deadline, so a laptop that slept through
// the wait still reports a state that settled meanwhile.
func (w *waiter) run(ctx context.Context, out io.Writer) int {
	deadline := w.now().Add(w.timeout)
	var appID string
	last := ""
	for {
		var err error
		if appID == "" {
			appID, err = w.api.appID(ctx, w.bundleID)
		}
		var u *buildUpload
		var bs string
		if err == nil {
			u, bs, err = w.api.upload(ctx, appID, w.build, w.platform)
		}
		switch {
		case err != nil && fatal(err):
			fmt.Fprintf(w.log, "ascwait: %v\nbuild %s: state unknown\n", err, w.build)
			return exitUnknown
		case err != nil:
			fmt.Fprintf(w.log, "ascwait: %v; trying again\n", err)
		case u == nil:
			if last != "not listed" {
				fmt.Fprintf(w.log, "build %s: not listed yet\n", w.build)
				last = "not listed"
			}
		default:
			st := u.Attributes.State
			// A COMPLETE upload passed Apple's checks; TestFlight shows it
			// once its build is VALID.
			switch {
			case st.State == "COMPLETE" && bs == "VALID":
				fmt.Fprintf(out, "build %s (%s): processed and valid\n", w.build, u.Attributes.Version)
				printDetails(out, "warning", st.Warnings)
				return exitComplete
			case st.State == "FAILED", bs == "FAILED", bs == "INVALID":
				fmt.Fprintf(w.log, "build %s (%s): App Store Connect rejected it (upload %s, build %s)\n",
					w.build, u.Attributes.Version, st.State, orNone(bs))
				printDetails(w.log, "error", st.Errors)
				printDetails(w.log, "warning", st.Warnings)
				return exitFailed
			}
			if now := "upload " + st.State + ", build " + orNone(bs); now != last {
				fmt.Fprintf(w.log, "build %s: %s\n", w.build, now)
				last = now
			}
		}
		if !w.now().Before(deadline) {
			fmt.Fprintf(w.log, "build %s: still not processed after %s; state unknown\n", w.build, w.timeout)
			return exitUnknown
		}
		w.sleep(w.interval)
	}
}

func orNone(s string) string {
	if s == "" {
		return "none yet"
	}
	return s
}

func printDetails(out io.Writer, kind string, ds []detail) {
	for _, d := range ds {
		fmt.Fprintf(out, "  %s %s: %s\n", kind, d.Code, d.Description)
	}
}
