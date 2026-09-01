//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package agenttest is a test harness for driving agent.Run from outside the agent
// package: it consolidates the idioms each internal package had grown its own copy
// of (a fake fisk application, a scripted llm.Provider, a recording agent.Events, a
// scripted toolkit.Prompter, and config builders) so a caller can stand up a run
// without reaching into internals.
//
// Constructors come in two forms. A New form takes a testing.TB and calls Fatalf where
// construction can fail; NewOTLPReceiver also registers the Close its listener needs. A
// Build form takes no testing.TB and returns an error instead, leaving the caller to call
// Close on the one fake that holds a resource. A func Example is handed no testing.TB, so
// it reaches the harness through the Build form.
//
// The package imports testing and net/http/httptest, so a production binary that links it
// links both.
package agenttest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/choria-io/fisk"
)

// FakeApp is a runnable stand-in fisk application. Its --fisk-introspect output is
// the genuine introspection of a real fisk.Application, so the tool schemas the
// agent loads are real rather than hand-written JSON; every other invocation echoes
// its arguments one per line, so a tool call produces a deterministic result a test
// can assert against. Path is the value an agent config's application_path points at.
//
// The executable is a /bin/sh script, so it runs on Unix only. NewFakeApp skips the test
// on Windows and BuildFakeApp returns an error there, so a caller learns the platform is
// unsupported at construction rather than from an exec error wherever Path is later run.
type FakeApp struct {
	Path string
}

// NewFakeApp introspects app in-process to capture its genuine command model, then
// returns an executable that replays that model on --fisk-introspect and echoes its
// arguments otherwise. It sets a no-op Terminate on app, since --fisk-introspect
// would otherwise call os.Exit through fisk's default terminate.
//
// The executable is a /bin/sh script, so on Windows NewFakeApp skips the calling test.
//
// The script is written once per process for each distinct command model and shared
// by every later call that produces the same one, so a suite standing up the same
// application in many tests writes one file rather than one per test. Nothing in the file
// is per-test: it is read-only once written, and the working directory it reports is the
// calling run's own.
func NewFakeApp(tb testing.TB, app *fisk.Application) *FakeApp {
	tb.Helper()

	if runtime.GOOS == "windows" {
		tb.Skipf("agenttest: %v", errFakeAppWindows)
	}

	fake, err := BuildFakeApp(app)
	if err != nil {
		tb.Fatalf("agenttest: %v", err)
	}

	return fake
}

// BuildFakeApp is NewFakeApp without a testing.TB, for a func Example or any other
// caller outside a test. It returns an error where NewFakeApp fails the test: app
// could not be introspected, or the script could not be written. With no testing.TB to
// skip, Windows comes back as an error too, and a caller that wants to skip instead
// checks runtime.GOOS before calling.
//
// Every call producing the same command model shares one script, kept for the life of
// the process.
//
// Several goroutines may call BuildFakeApp at once. The one thing it cannot share the
// process with is another writer to stdout: capturing the model means pointing os.Stdout
// at a pipe, because fisk writes introspection there and takes no writer, so anything
// else printing to stdout during the call lands in the captured model. BuildFakeApp
// returns an error when that happens rather than handing back a fake application whose
// --fisk-introspect output no longer parses.
func BuildFakeApp(app *fisk.Application) (*FakeApp, error) {
	if runtime.GOOS == "windows" {
		return nil, errFakeAppWindows
	}

	model, err := introspectJSON(app)
	if err != nil {
		return nil, err
	}

	appPath, err := fakeAppExecutable(model)
	if err != nil {
		return nil, err
	}

	return &FakeApp{Path: appPath}, nil
}

// errFakeAppWindows is what NewFakeApp skips with and BuildFakeApp returns on Windows.
// It stays unexported: a caller branching on the platform reads runtime.GOOS, which it
// can do before calling and which needs nothing from this package.
var errFakeAppWindows = fmt.Errorf("the fake application is a /bin/sh script and runs on Unix only")

var (
	fakeAppRootOnce sync.Once
	fakeAppRoot     string
	fakeAppRootErr  error

	fakeAppMu    sync.Mutex
	fakeAppPaths = map[string]string{}

	introspectMu sync.Mutex
)

// fakeAppExecutable returns the path of the executable that replays model, writing it and
// the model beside it on the first call for that model. Entries are keyed on the model's
// content hash and each gets its own subdirectory, so two applications never overwrite one
// another and two calls carrying the same model produce byte-identical files. Callers
// reach it from whatever goroutine they run on, so a mutex covers both the map and the
// writes.
//
// The root directory is created once per process and is not registered with a testing.TB,
// since it outlives every individual test. The operating system reclaims it as it does any
// temporary directory a test process leaves behind.
func fakeAppExecutable(model []byte) (string, error) {
	sum := sha256.Sum256(model)
	key := hex.EncodeToString(sum[:])

	fakeAppMu.Lock()
	defer fakeAppMu.Unlock()

	appPath, ok := fakeAppPaths[key]
	if ok {
		return appPath, nil
	}

	fakeAppRootOnce.Do(func() {
		fakeAppRoot, fakeAppRootErr = os.MkdirTemp("", "agenttest-fakeapp")
	})
	if fakeAppRootErr != nil {
		return "", fmt.Errorf("creating fake application directory: %w", fakeAppRootErr)
	}

	dir := filepath.Join(fakeAppRoot, key)
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return "", fmt.Errorf("creating fake application directory: %w", err)
	}

	jsonPath := filepath.Join(dir, "introspect.json")
	err = os.WriteFile(jsonPath, model, 0o600)
	if err != nil {
		return "", fmt.Errorf("writing introspect model: %w", err)
	}

	// On --fisk-introspect the binary replays the captured model; otherwise it reports
	// its working directory (so a test can observe the per-run ToolWorkDir) and echoes
	// each argument on its own line, matching the long-standing fake-application idiom so
	// a tool call's output is predictable. The shell reads $PWD from the environment,
	// which fisktool.RunCommand sets to the directory it was handed, so the reported path
	// carries the caller's spelling rather than the one os.Getwd resolves symlinks to.
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--fisk-introspect\" ]; then\n  cat %q\n  exit 0\nfi\nprintf 'PWD=%%s\\n' \"$PWD\"\nfor a in \"$@\"; do printf '%%s\\n' \"$a\"; done\n", jsonPath)

	appPath = filepath.Join(dir, "app")
	err = os.WriteFile(appPath, []byte(script), 0o700)
	if err != nil {
		return "", fmt.Errorf("writing fake application: %w", err)
	}

	fakeAppPaths[key] = appPath

	return appPath, nil
}

// introspectJSON drives app's real --fisk-introspect handler in-process and returns
// the JSON it writes, the same document the agent would read over the process
// boundary, so the captured schemas are precomputed exactly as production sees them.
//
// fisk's introspect action sets the application writer to os.Stdout itself and then calls
// fmt.Println, so pointing os.Stdout at a pipe is the only way to read the document
// in-process. introspectMu serializes that swap: two goroutines building fake
// applications at once take it in turn, rather than restoring one another's saved
// os.Stdout. Anything else in the process printing to stdout during the parse writes its
// bytes into the same pipe, and they arrive interleaved with the model. So the capture is
// decoded before it is returned, and a document that no longer parses is reported here
// instead of becoming a fake application whose --fisk-introspect output is garbage.
func introspectJSON(app *fisk.Application) ([]byte, error) {
	// --fisk-introspect terminates the process; make that a no-op so the parse returns.
	app.Terminate(func(int) {})

	introspectMu.Lock()
	defer introspectMu.Unlock()

	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("creating introspect pipe: %w", err)
	}

	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	// Read concurrently so a large model cannot fill the pipe and block the write.
	captured := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(r)
		captured <- data
	}()

	// Closing the write end is what ends the reader goroutine, so every path below
	// closes it and drains the channel. An error return that skipped either would
	// leave the goroutine parked on a pipe nothing writes to and both descriptors
	// open, once per call, in a caller that carries on rather than failing a test.
	defer func() {
		r.Close()
	}()

	_, parseErr := app.Parse([]string{"--fisk-introspect"})
	closeErr := w.Close()
	data := <-captured

	if parseErr != nil {
		return nil, fmt.Errorf("introspecting fake application: %w", parseErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("closing introspect pipe: %w", closeErr)
	}

	// fisk emits one JSON document and nothing else, so anything that does not decode as
	// the model carries bytes another writer to os.Stdout put in the pipe during the
	// parse. The error names that, since it is the only way the capture goes wrong.
	var model fisk.ApplicationModel
	err = json.Unmarshal(data, &model)
	if err != nil {
		return nil, fmt.Errorf("introspecting fake application: the captured stdout is not an application model, so something else in this process printed to stdout during the parse: %w", err)
	}

	return data, nil
}
