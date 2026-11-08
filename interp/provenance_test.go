// Regression tests for the optional CSV source audit (provenance tracking).

package interp

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/benhoyt/goawk/parser"
)

// auditRun holds the outputs of one audited interpreter run.
type auditRun struct {
	stdout string
	audit  []auditEntry
	err    error
}

// runAudit parses and executes src with CSV input/output and source
// auditing enabled, returning stdout, the decoded side file, and error.
func runAudit(t *testing.T, src, input string, configure func(*Config)) auditRun {
	t.Helper()
	prog, err := parser.ParseProgram([]byte(src), nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	stdout := &bytes.Buffer{}
	auditBuf := &bytes.Buffer{}
	config := &Config{
		Stdin:         strings.NewReader(input),
		Output:        stdout,
		SourceAudit:   auditBuf,
		InputMode:     CSVMode,
		OutputMode:    CSVMode,
		NewlineOutput: RawNewlineMode, // deterministic \n in expectations
	}
	if configure != nil {
		configure(config)
	}
	_, err = ExecProgram(prog, config)
	result := auditRun{stdout: stdout.String(), err: err}
	if err == nil {
		dec := json.NewDecoder(auditBuf)
		for dec.More() {
			var entry auditEntry
			if err := dec.Decode(&entry); err != nil {
				t.Fatalf("decoding audit side file: %v", err)
			}
			result.audit = append(result.audit, entry)
		}
	}
	return result
}

func src(reader, record, column int) auditSource {
	return auditSource{Reader: reader, File: "-", Record: record, Column: column}
}

func srcFile(file string, reader, record, column int) auditSource {
	return auditSource{Reader: reader, File: file, Record: record, Column: column}
}

// sourcesOf returns the sources of the (1-based) output field of the
// (1-based) output record.
func sourcesOf(entries []auditEntry, record, field int) []auditSource {
	for _, e := range entries {
		if e.Record == record && e.Field == field {
			return e.Sources
		}
	}
	return nil
}

func assertSources(t *testing.T, entries []auditEntry, record, field int, want []auditSource) {
	t.Helper()
	got := sourcesOf(entries, record, field)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("record %d field %d sources: got %#v, want %#v", record, field, got, want)
	}
}

func joinSources(sources []auditSource) string {
	b := strings.Builder{}
	for _, s := range sources {
		b.WriteString(s.File)
		b.WriteByte(':')
		b.WriteByte(byte('0' + s.Record))
		b.WriteByte(':')
		b.WriteByte(byte('0' + s.Column))
	}
	return b.String()
}

// Basic tracking: output bytes are unchanged and each printed field
// points at the input cell it came from.
func TestAuditBasic(t *testing.T) {
	r := runAudit(t, `{ print $2, $1 }`, "a,b\nc,d\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "b,a\nd,c\n" {
		t.Fatalf("stdout changed: %q", r.stdout)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 2)})
	assertSources(t, r.audit, 1, 2, []auditSource{src(1, 1, 1)})
	assertSources(t, r.audit, 2, 1, []auditSource{src(1, 2, 2)})
	assertSources(t, r.audit, 2, 2, []auditSource{src(1, 2, 1)})
}

// TSV mode works the same, with tab-separated logical fields.
func TestAuditTSV(t *testing.T) {
	r := runAudit(t, `{ print $2 }`, "a\tb\n", func(c *Config) {
		c.InputMode = TSVMode
		c.OutputMode = TSVMode
	})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "b\n" {
		t.Fatalf("stdout changed: %q", r.stdout)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 2)})
}

// Constants have empty provenance; repeated cells are de-duplicated.
func TestAuditConstantsAndDedup(t *testing.T) {
	r := runAudit(t, `{ x=$1; y=$1; print "label", x, y }`, "a,b\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{})
	assertSources(t, r.audit, 1, 2, []auditSource{src(1, 1, 1)})
	assertSources(t, r.audit, 1, 3, []auditSource{src(1, 1, 1)})
}

// Aggregation merges the sources of every value actually accumulated.
func TestAuditAggregation(t *testing.T) {
	r := runAudit(t, `{ sum += $2 } END { print sum }`, "a,1\nb,2\nc,3\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "6\n" {
		t.Fatalf("stdout changed: %q", r.stdout)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{
		src(1, 1, 2), src(1, 2, 2), src(1, 3, 2),
	})
}

// Aggregation through string concatenation.
func TestAuditAggregationConcat(t *testing.T) {
	r := runAudit(t, `{ s = s $1 } END { print s }`, "a,x\nb,y\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{
		src(1, 1, 1), src(1, 2, 1),
	})
}

// Plain assignment replaces sources; incremental assignment merges the
// old sources.
func TestAuditOverwriteAndIncrement(t *testing.T) {
	r := runAudit(t, `
{ x = $1; x = $2; print x }
{ y += $1; y += $2; print y }
`, "a,b\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	// x was overwritten: only $2's source remains.
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 2)})
	// y merged $1 then $2 sources.
	assertSources(t, r.audit, 2, 1, []auditSource{src(1, 1, 1), src(1, 1, 2)})
}

// Writing one field rebuilds $0 from the retained field sources.
func TestAuditFieldWriteBack(t *testing.T) {
	r := runAudit(t, `{ $1 = $3; print $1, $2, $0 }`, "a,b,c\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 3)})
	assertSources(t, r.audit, 1, 2, []auditSource{src(1, 1, 2)})
	// $0 keeps $2 and $3 (the new $1 is sourced from $3, de-duplicated).
	assertSources(t, r.audit, 1, 3, []auditSource{src(1, 1, 2), src(1, 1, 3)})
}

// Assigning $0 makes re-split fields inherit the whole right-hand
// source, without byte-level tracking. Fields beyond the re-split count
// are empty (and source-less).
func TestAuditRebuildDollarZero(t *testing.T) {
	r := runAudit(t, `{ $0 = $2; print $1, $2 }`, "a,b,c\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "b,\n" {
		t.Fatalf("stdout changed: %q", r.stdout)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 2)})
	assertSources(t, r.audit, 1, 2, []auditSource{}) // out of range after re-split
}

// NF shrink drops sources; NF grow adds empty-source fields.
func TestAuditNFAdjust(t *testing.T) {
	r := runAudit(t, `{ NF = 2; print $1, $2 } { NF = 4; print $4 }`, "a,b,c\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1)})
	assertSources(t, r.audit, 1, 2, []auditSource{src(1, 1, 2)})
	assertSources(t, r.audit, 2, 1, []auditSource{})
}

// A quoted field spanning physical lines is one logical data record.
func TestAuditMultilineQuotedRecord(t *testing.T) {
	input := "\"a\nx\",b\nc,d\n"
	r := runAudit(t, `{ print NR, $1 }`, input, nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "1,\"a\nx\"\n2,c\n" {
		t.Fatalf("stdout changed: %q", r.stdout)
	}
	assertSources(t, r.audit, 1, 2, []auditSource{src(1, 1, 1)})
	assertSources(t, r.audit, 2, 2, []auditSource{src(1, 2, 1)})
}

// getline advances the input; values saved before the advance keep
// their original record's sources.
func TestAuditGetlineAdvance(t *testing.T) {
	r := runAudit(t, `NR==1 { x=$1; getline; print x, $1 }`, "a,b\nc,d\ne,f\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "a,c\n" {
		t.Fatalf("stdout changed: %q", r.stdout)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1)})
	assertSources(t, r.audit, 1, 2, []auditSource{src(1, 2, 1)})
}

// getline into a variable gives it the new record's $0 sources.
func TestAuditGetlineVariable(t *testing.T) {
	r := runAudit(t, `NR==1 { getline x; print x }`, "a,b\nc,d\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 2, 1), src(1, 2, 2)})
}

// Rewriting NR/FNR/FILENAME must not change cell identity.
func TestAuditIgnoresScriptSpecialVars(t *testing.T) {
	r := runAudit(t, `{ NR=999; FNR=888; FILENAME="fake"; print $1 }`, "a,b\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1)})
}

// Multiple input files get distinct reader instances while logical
// record numbers restart per reader.
func TestAuditMultipleReaders(t *testing.T) {
	prog, err := parser.ParseProgram([]byte(`{ print $1 }`), nil)
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{
		"f1.csv": &fstest.MapFile{Data: []byte("a,b\n")},
		"f2.csv": &fstest.MapFile{Data: []byte("c,d\n")},
	}
	stdout := &bytes.Buffer{}
	auditBuf := &bytes.Buffer{}
	config := &Config{
		Args:        []string{"f1.csv", "f2.csv"},
		Output:      stdout,
		FileSystem:  fsys,
		SourceAudit: auditBuf,
		InputMode:   CSVMode,
		OutputMode:  CSVMode,
	}
	if _, err := ExecProgram(prog, config); err != nil {
		t.Fatal(err)
	}
	var entries []auditEntry
	dec := json.NewDecoder(auditBuf)
	for dec.More() {
		var e auditEntry
		if err := dec.Decode(&e); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	assertSources(t, entries, 1, 1, []auditSource{srcFile("f1.csv", 1, 1, 1)})
	assertSources(t, entries, 2, 1, []auditSource{srcFile("f2.csv", 2, 1, 1)})
}

// Header names in FIELDS have no sources; @named fields reference data
// cells (header rows don't count as logical data records).
func TestAuditHeaderAndNamedFields(t *testing.T) {
	r := runAudit(t, `{ print @"name", @"age", FIELDS[1] }`,
		"name,age\nBob,42\n", func(c *Config) {
			c.CSVInput.Header = true
		})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "Bob,42,name\n" {
		t.Fatalf("stdout changed: %q", r.stdout)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1)})
	assertSources(t, r.audit, 1, 2, []auditSource{src(1, 1, 2)})
	assertSources(t, r.audit, 1, 3, []auditSource{}) // header name
}

// Array element values don't absorb index sources, but reads merge the
// current index sources.
func TestAuditArrayReadWrite(t *testing.T) {
	// The stored element carries only the value's sources.
	r := runAudit(t, `{ a[$1]=$2 } END { print a["k2"] }`,
		"k1,v1,x\nk2,v2,x\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 2, 2)})

	// A read through an indexed expression merges that index's sources.
	r = runAudit(t, `{ a[$1]=$2 } END { idx=$1; print a[idx] }`,
		"k1,v1,x\nk2,v2,x\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 2, 1), src(1, 2, 2)})
}

// for-in keys carry the union of their lifetime index sources; delete
// clears both value and key sources.
func TestAuditForInAndDelete(t *testing.T) {
	r := runAudit(t, `{ a[$1]=$2 } END { for (k in a) print k }`,
		"k1,v1\nk2,v2\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	// Map iteration order is unspecified, compare as a set.
	got := map[string][]auditSource{}
	for _, e := range r.audit {
		got[joinSources(e.Sources)] = e.Sources
	}
	want := map[string][]auditSource{
		joinSources([]auditSource{src(1, 1, 1)}): {src(1, 1, 1)},
		joinSources([]auditSource{src(1, 2, 1)}): {src(1, 2, 1)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("for-in key sources: got %#v, want %#v", got, want)
	}

	// After delete, the surviving key keeps only its own sources.
	r = runAudit(t, `{ a[$1]=$2 } END { delete a[$1]; for (k in a) print k }`,
		"k1,v1\nk2,v2\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.audit) != 1 {
		t.Fatalf("expected one surviving key, got %d entries", len(r.audit))
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1)})
}

// "in" merges the current index and the queried key's sources.
func TestAuditInOperator(t *testing.T) {
	r := runAudit(t, `{ a[$1]=$2 } END { print ($1 in a) }`,
		"k1,v1\nk2,v2\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 2, 1)})
}

// Array length merges the surviving keys' sources.
func TestAuditArrayLength(t *testing.T) {
	r := runAudit(t, `{ a[$1]=$2 } END { print length(a) }`,
		"k1,v1\nk2,v2\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "2\n" {
		t.Fatalf("stdout changed: %q", r.stdout)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1), src(1, 2, 1)})
}

// split results and the count inherit the scalar argument sources.
func TestAuditSplit(t *testing.T) {
	r := runAudit(t, `{ n=split($1, arr, "-"); print arr[2], n }`,
		"a-b-c,x\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "b,3\n" {
		t.Fatalf("stdout changed: %q", r.stdout)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1)})
	assertSources(t, r.audit, 1, 2, []auditSource{src(1, 1, 1)})
}

// sub write-back and match-derived variables merge actual operands.
func TestAuditSubAndMatch(t *testing.T) {
	r := runAudit(t, `{ sub(/x/, "y", $1); print $1 }`, "axb,c\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "ayb\n" {
		t.Fatalf("stdout changed: %q", r.stdout)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1)})

	// gsub with implicit $0: every re-split field inherits all record sources.
	r = runAudit(t, `{ gsub("x", "y"); print $1 }`, "axb,cx\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1), src(1, 1, 2)})

	// match() drives RSTART from the matched operand's sources.
	r = runAudit(t, `{ match($1, /x/); print RSTART }`, "axb,c\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1)})
}

// User functions track actual passed/returned values; unused arguments
// are not merged into the return value.
func TestAuditUserFunction(t *testing.T) {
	awkSrc := `function f(a, b) { return a } { print f($1, $2) }`
	r := runAudit(t, awkSrc, "a,b\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1)})

	// Values returning through locals still carry their sources.
	awkSrc = `function g(a) { a = a $2; return a } { print g($1) }`
	r = runAudit(t, awkSrc, "a,b\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1), src(1, 1, 2)})
}

// Short-circuiting operators don't count the unevaluated side.
func TestAuditShortCircuit(t *testing.T) {
	r := runAudit(t, `{ x = (0 && $1); y = (1 || $1); print x, y }`, "a,b\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{})
	assertSources(t, r.audit, 1, 2, []auditSource{})

	r = runAudit(t, `{ z = (1 && $1); print z }`, "a,b\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertSources(t, r.audit, 1, 1, []auditSource{src(1, 1, 1)})
}

// Bare print and printf don't emit side-file entries; only "print" with
// arguments to stdout does.
func TestAuditNoSideFileForBarePrintAndPrintf(t *testing.T) {
	r := runAudit(t, `{ print; printf "%s", $1 }`, "a,b\n", nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.audit) != 0 {
		t.Fatalf("expected no audit entries, got %d", len(r.audit))
	}
}

// Unsupported operations are refused explicitly rather than producing
// incomplete provenance.
func TestAuditUnsupportedRefused(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
		conf func(*Config)
	}{
		{"rand", `BEGIN { print rand() }`, "rand()", nil},
		{"srand", `BEGIN { srand() }`, "srand()", nil},
		{"srand-seed", `BEGIN { srand(1) }`, "srand()", nil},
		{"system", `BEGIN { print system("echo hi") }`, "system()", nil},
		{"print-redirect", `BEGIN { print "x" >"out.txt" }`, "redirected print", nil},
		{"printf-redirect", `BEGIN { printf "x" >"out.txt" }`, "redirected printf", nil},
		{"getline-file", `BEGIN { getline x <"in.txt" }`, "redirected getline", nil},
		{"getline-pipe", `BEGIN { "echo x" | getline x }`, "getline from a command", nil},
		{"native", `BEGIN { print f() }`, "native function", func(c *Config) {
			c.Funcs = map[string]any{"f": func() int { return 1 }}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Native functions must be registered at parse time too.
			var parserConfig *parser.ParserConfig
			if tc.conf != nil {
				cfg := &Config{}
				tc.conf(cfg)
				if cfg.Funcs != nil {
					parserConfig = &parser.ParserConfig{Funcs: cfg.Funcs}
				}
			}
			prog, err := parser.ParseProgram([]byte(tc.src), parserConfig)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			auditBuf := &bytes.Buffer{}
			config := &Config{
				Output:        &bytes.Buffer{},
				SourceAudit:   auditBuf,
				InputMode:     CSVMode,
				OutputMode:    CSVMode,
				NewlineOutput: RawNewlineMode,
			}
			if tc.conf != nil {
				tc.conf(config)
			}
			_, err = ExecProgram(prog, config)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "source audit") {
				t.Fatalf("error %q does not mention source audit / %q", err, tc.want)
			}
		})
	}
}

// Enabling the audit outside CSV/TSV modes is rejected up front.
func TestAuditModeValidation(t *testing.T) {
	prog, err := parser.ParseProgram([]byte(`{ print $1 }`), nil)
	if err != nil {
		t.Fatal(err)
	}
	auditBuf := &bytes.Buffer{}
	_, err = ExecProgram(prog, &Config{
		Stdin:       strings.NewReader("a\n"),
		Output:      &bytes.Buffer{},
		SourceAudit: auditBuf,
		OutputMode:  CSVMode,
	})
	if err == nil || !strings.Contains(err.Error(), "input mode") {
		t.Fatalf("expected input mode error, got %v", err)
	}

	_, err = ExecProgram(prog, &Config{
		Stdin:       strings.NewReader("a\n"),
		Output:      &bytes.Buffer{},
		SourceAudit: auditBuf,
		InputMode:   CSVMode,
	})
	if err == nil || !strings.Contains(err.Error(), "output mode") {
		t.Fatalf("expected output mode error, got %v", err)
	}
}

// With auditing disabled (nil side file), all behavior is unchanged and
// no provenance nodes are reachable.
func TestAuditDisabledKeepsBehavior(t *testing.T) {
	prog, err := parser.ParseProgram([]byte(`{ sum += $2; print $1, sum }`), nil)
	if err != nil {
		t.Fatal(err)
	}
	stdout := &bytes.Buffer{}
	config := &Config{
		Stdin:         strings.NewReader("a,1\nb,2\n"),
		Output:        stdout,
		InputMode:     CSVMode,
		OutputMode:    CSVMode,
		NewlineOutput: RawNewlineMode,
	}
	status, err := ExecProgram(prog, config)
	if err != nil {
		t.Fatal(err)
	}
	if status != 0 || stdout.String() != "a,1\nb,3\n" {
		t.Fatalf("unexpected output: status=%d out=%q", status, stdout.String())
	}
}
