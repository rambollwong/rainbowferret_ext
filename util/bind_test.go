package util

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rambollwong/rainbowferret/types"
)

type bindReq struct {
	Name    string   `json:"name" form:"name" xml:"name" yaml:"name"`
	Age     int      `json:"age" form:"age" xml:"age" yaml:"age"`
	Active  bool     `json:"active" form:"active" xml:"active" yaml:"active"`
	Tags    []string `form:"tags" yaml:"tags"`
	Default string   // no tag, falls back to field name
}

func TestBindJSON(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"ramboll","age":18,"active":true}`))
	r.Header.Set("Content-Type", "application/json")

	var v bindReq
	if err := Bind(r, &v); err != nil {
		t.Fatalf("Bind error: %v", err)
	}
	if v.Name != "ramboll" || v.Age != 18 || !v.Active {
		t.Fatalf("unexpected struct: %+v", v)
	}
}

func TestBindForm(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("name=ramboll&age=18&active=true"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var v bindReq
	if err := Bind(r, &v); err != nil {
		t.Fatalf("Bind error: %v", err)
	}
	if v.Name != "ramboll" || v.Age != 18 || !v.Active {
		t.Fatalf("unexpected struct: %+v", v)
	}
}

func TestBindQueryFallback(t *testing.T) {
	// No Content-Type: falls back to query/form binding, like Gin.
	r := httptest.NewRequest(http.MethodGet, "/?name=ramboll&age=18&active=true", nil)

	var v bindReq
	if err := Bind(r, &v); err != nil {
		t.Fatalf("Bind error: %v", err)
	}
	if v.Name != "ramboll" || v.Age != 18 || !v.Active {
		t.Fatalf("unexpected struct: %+v", v)
	}
}

func TestBindUnsupportedMediaType(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("plain text"))
	r.Header.Set("Content-Type", "text/plain")

	var v bindReq
	err := Bind(r, &v)
	if err == nil {
		t.Fatal("expected error for unsupported media type")
	}
	he, ok := err.(*types.HTTPError)
	if !ok {
		t.Fatalf("expected *types.HTTPError, got %T", err)
	}
	if he.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", he.Code)
	}
}

func TestDecodeFormURLEncoded(t *testing.T) {
	body := "name=ramboll&age=18&active=true&tags=a&tags=b&Default=x"
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var v bindReq
	if err := DecodeForm(r, &v); err != nil {
		t.Fatalf("DecodeForm error: %v", err)
	}
	if v.Name != "ramboll" || v.Age != 18 || !v.Active {
		t.Fatalf("unexpected struct: %+v", v)
	}
	if len(v.Tags) != 2 || v.Tags[0] != "a" || v.Tags[1] != "b" {
		t.Fatalf("unexpected tags: %v", v.Tags)
	}
	if v.Default != "x" {
		t.Fatalf("unexpected Default: %q", v.Default)
	}
}

func TestDecodeFormMultipart(t *testing.T) {
	body := "--b\r\nContent-Disposition: form-data; name=\"name\"\r\n\r\nramboll\r\n--b\r\nContent-Disposition: form-data; name=\"age\"\r\n\r\n25\r\n--b--\r\n"
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")

	var v bindReq
	if err := DecodeForm(r, &v); err != nil {
		t.Fatalf("DecodeForm error: %v", err)
	}
	if v.Name != "ramboll" || v.Age != 25 {
		t.Fatalf("unexpected struct: %+v", v)
	}
}

func TestDecodeFormIntoValues(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("a=1&b=2&b=3"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var vals url.Values
	if err := DecodeForm(r, &vals); err != nil {
		t.Fatalf("DecodeForm error: %v", err)
	}
	if vals.Get("a") != "1" || len(vals["b"]) != 2 {
		t.Fatalf("unexpected values: %v", vals)
	}
}

func TestDecodeFormRejectsNonForm(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}`))
	r.Header.Set("Content-Type", "application/json")

	var v bindReq
	err := DecodeForm(r, &v)
	if err == nil {
		t.Fatal("expected error for non-form content type")
	}
	he, ok := err.(*types.HTTPError)
	if !ok {
		t.Fatalf("expected *types.HTTPError, got %T", err)
	}
	if he.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", he.Code)
	}
}

func TestBindXML(t *testing.T) {
	body := `<bindReq><name>ramboll</name><age>18</age><active>true</active></bindReq>`
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/xml")

	var v bindReq
	if err := Bind(r, &v); err != nil {
		t.Fatalf("Bind error: %v", err)
	}
	if v.Name != "ramboll" || v.Age != 18 || !v.Active {
		t.Fatalf("unexpected struct: %+v", v)
	}
}

func TestDecodeXMLRejectsNonXML(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}`))
	r.Header.Set("Content-Type", "application/json")

	var v bindReq
	err := DecodeXML(r, &v)
	if err == nil {
		t.Fatal("expected error for non-xml content type")
	}
	he, ok := err.(*types.HTTPError)
	if !ok {
		t.Fatalf("expected *types.HTTPError, got %T", err)
	}
	if he.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", he.Code)
	}
}

func TestBindYAML(t *testing.T) {
	body := "name: ramboll\nage: 18\nactive: true\n"
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/yaml")

	var v bindReq
	if err := Bind(r, &v); err != nil {
		t.Fatalf("Bind error: %v", err)
	}
	if v.Name != "ramboll" || v.Age != 18 || !v.Active {
		t.Fatalf("unexpected struct: %+v", v)
	}
}

func TestDecodeYAMLRejectsNonYAML(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}`))
	r.Header.Set("Content-Type", "application/json")

	var v bindReq
	err := DecodeYAML(r, &v)
	if err == nil {
		t.Fatal("expected error for non-yaml content type")
	}
	he, ok := err.(*types.HTTPError)
	if !ok {
		t.Fatalf("expected *types.HTTPError, got %T", err)
	}
	if he.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", he.Code)
	}
}
