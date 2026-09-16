// ═══ 更新日志 ═══
// 2026-09-16：验证数字字面量保真、完整 JSON 边界与类型校验，防止共享解码入口改变请求语义。
package jsonutil

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestDecodePreservesNumberLiterals(t *testing.T) {
	const raw = `{"values":[9007199254740993,-9007199254740993,-0,0.10000000000000001,1.2300e+04,1e999],"enabled":false,"text":" keep ","optional":null}`
	var got map[string]any
	if err := Decode([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	want := []any{
		json.Number("9007199254740993"), json.Number("-9007199254740993"),
		json.Number("-0"), json.Number("0.10000000000000001"),
		json.Number("1.2300e+04"), json.Number("1e999"),
	}
	if !reflect.DeepEqual(got["values"], want) {
		t.Fatalf("numeric values changed: got %#v, want %#v", got["values"], want)
	}
	if got["enabled"] != false || got["text"] != " keep " || got["optional"] != nil {
		t.Fatalf("ordinary fields changed: %#v", got)
	}
	if _, present := got["optional"]; !present {
		t.Fatal("explicit null field lost")
	}
	encoded, err := json.Marshal(got["values"])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `[9007199254740993,-9007199254740993,-0,0.10000000000000001,1.2300e+04,1e999]` {
		t.Fatalf("numeric serialization changed: %s", encoded)
	}
}

func TestDecodeRejectsTrailingJSONAndGarbage(t *testing.T) {
	for _, raw := range []string{
		`{} {}`, `{} null`, `{} 0`, `{} garbage`, `{} {`,
		`{"id":9007199254740993} false`, "", `{"id":`,
	} {
		t.Run(raw, func(t *testing.T) {
			var got any
			if err := Decode([]byte(raw), &got); err == nil {
				t.Fatalf("invalid complete JSON document accepted: %q", raw)
			}
		})
	}
	var got map[string]any
	if err := Decode([]byte(" \t{\"id\":9007199254740993}\n\r\t "), &got); err != nil {
		t.Fatalf("trailing whitespace rejected: %v", err)
	}
}

func TestDecodeRetainsTypedFieldValidation(t *testing.T) {
	var got struct {
		Limit int            `json:"limit"`
		Extra map[string]any `json:"extra"`
	}
	if err := Decode([]byte(`{"limit":4096,"extra":{"id":9007199254740993}}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Limit != 4096 || got.Extra["id"] != json.Number("9007199254740993") {
		t.Fatalf("typed decoding changed: %#v", got)
	}
	if err := Decode([]byte(`{"limit":0.5}`), &got); err == nil {
		t.Fatal("fractional integer field accepted")
	}
	if err := Decode([]byte(`{"limit":"4096"}`), &got); err == nil {
		t.Fatal("string integer field accepted")
	}
}
