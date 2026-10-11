package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file locks down the **capability boundary** of the generic webhook template.
//
// This is the only place in the package where "a user-provided string is evaluated as code", so we must be
// explicit about what it can and cannot do, and pin these properties down with tests — otherwise someone
// could later add a method to the template context or add a readFile to the FuncMap, silently expanding the
// capability surface, and the diff would look like just a harmless little function.

// TestTemplateContextHasNoMethods is the most important one.
//
// text/template calls exported methods ({{.Foo}} can read a field or call a method). So the template context
// merely being able to reach **any** type with exported methods exposes those methods to the template author.
// This feature's context is deliberately all plain data (only exported fields, zero methods).
//
// If this fails: someone added a method to webhookTemplateData / webhookItem.
// Before deciding to allow it, first think about whether that method could be used by the template to read
// something that should not be exposed.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposes %d methods (%s): text/template can call them, "+
				"which opens those methods' capabilities to the template author", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal locks down the set of functions exposed to templates.
//
// Every extra function in the FuncMap is an extra capability. Currently only json / jsons exist, used to serialize
// a value into a JSON fragment — they cannot read files, send requests, or execute commands.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the template function set changed: got %v, expected %v. Before adding a function, confirm it does not expand the capability surface"+
			"(it must not read/write files, make network requests, or execute commands)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers out-of-bounds access in the template:
// accessing something that does not exist must fail rather than echo something back; and the failure must
// not leak internal data.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("accessing a nonexistent field should report an error")
	}
	// The error must not contain the real content of the template context (finding title / summary).
	for _, leak := range []string{"SQL Injection", "Parameter id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("template error leaked the message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently: a template mistake is a config error, and retries will not self-heal.
// If it were judged retryable, a single bad template would make every delivery waste three rounds of backoff.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("a template syntax error should be caught at save time")
	}
	// Even if validation is bypassed and it is delivered directly, it must be judged permanent rather than retried over and over.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a bad template should be judged permanent, got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON covers the constraint that "the rendered template result must be valid JSON".
// It also incidentally blocks the use of "generate plain text via a template to trigger other protocols".
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// A valid template passes.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("a valid template should pass validation: %v", err)
	}
	// Rendering non-JSON must be rejected (rather than sent out as-is).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("rendering non-JSON should be judged permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("the error message should explain it is a JSON problem, got %v", err)
	}
}
