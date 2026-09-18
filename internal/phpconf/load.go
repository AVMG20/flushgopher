package phpconf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// PHPBinary is the php executable used by the LoadConfig fallback.
var PHPBinary = "php"

// PHPTimeout bounds the php fallback.
var PHPTimeout = 30 * time.Second

const jsonMarker = "__MCC_JSON__"

// phpDumpCode prints a marker followed by the JSON encoding of the file's
// return value. The path is passed as $argv[1], never interpolated.
const phpDumpCode = `echo "\n` + jsonMarker + `", json_encode((require $argv[1]) ?? [], JSON_PRESERVE_ZERO_FRACTION | JSON_INVALID_UTF8_SUBSTITUTE | JSON_PARTIAL_OUTPUT_ON_ERROR);`

// LoadConfig evaluates a PHP config file that returns an array. It parses the
// file natively and falls back to running php when the native parser can't
// handle the file.
func LoadConfig(path string) (map[string]any, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	v, nerr := Parse(src)
	if nerr != nil {
		var perr error
		if v, perr = PHPEval(path); perr != nil {
			return nil, fmt.Errorf("%s: native parse failed: %v; php fallback failed: %v", path, nerr, perr)
		}
	}
	m, err := asMap(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// PHPEval runs php to evaluate the file and decodes the JSON encoding of its
// return value into the same value types Parse produces. Output printed by
// php (or a wrapper script) before the marker is ignored.
func PHPEval(path string) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), PHPTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, PHPBinary, "-r", phpDumpCode, "--", path)
	// Don't hang on the timeout when a child of php keeps stdout open.
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	out := stdout.Bytes()
	i := bytes.LastIndex(out, []byte(jsonMarker))
	if i < 0 {
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		if runErr != nil {
			return nil, fmt.Errorf("%s: %v: %s", PHPBinary, runErr, msg)
		}
		return nil, fmt.Errorf("%s: no output marker found: %s", PHPBinary, msg)
	}
	return DecodeJSON(out[i+len(jsonMarker):])
}

// DecodeJSON decodes JSON into the value types used by Parse: numbers
// become int64 when integral, float64 otherwise.
func DecodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decoding php json output: %w", err)
	}
	return convertJSON(v), nil
}

func convertJSON(v any) any {
	switch x := v.(type) {
	case json.Number:
		if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return n
		}
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	case map[string]any:
		for k, e := range x {
			x[k] = convertJSON(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = convertJSON(e)
		}
		return x
	}
	return v
}

func asMap(v any) (map[string]any, error) {
	switch x := v.(type) {
	case map[string]any:
		return x, nil
	case []any:
		m := make(map[string]any, len(x))
		for i, e := range x {
			m[strconv.Itoa(i)] = e
		}
		return m, nil
	}
	return nil, fmt.Errorf("config does not return an array (got %T)", v)
}
