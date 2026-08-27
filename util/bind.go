package util

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/rambollwong/rainbowferret/types"
	"github.com/rambollwong/rainbowferret/util"
	"gopkg.in/yaml.v3"
)

// Bind binds the request to v automatically according to the Content-Type header,
// like Gin's ShouldBind. It supports application/json, application/xml, text/xml,
// application/yaml, application/x-yaml, text/yaml, application/x-www-form-urlencoded
// and multipart/form-data; when Content-Type is missing it falls back to form/query
// binding. Unsupported media types return a 415 error.
// Bind 类似 Gin 的 ShouldBind，根据 Content-Type 头自动将请求绑定到 v，
// 支持 application/json、application/xml、text/xml、application/yaml、
// application/x-yaml、text/yaml、application/x-www-form-urlencoded 和
// multipart/form-data；缺少 Content-Type 时回退为表单/查询参数绑定；
// 不支持的媒体类型返回 415 错误。
func Bind(r *http.Request, v any) error {
	mediaType := mediaTypeOf(r)

	switch {
	case mediaType == "":
		if err := parseForm(r, mediaType); err != nil {
			return err
		}
		if err := fillFromValuesAndParams(r, r.Form, v); err != nil {
			return err
		}
		if vv, ok := v.(util.Validator); ok {
			return vv.Validate()
		}
		return nil
	case strings.EqualFold(mediaType, "application/json"):
		if err := DecodeJSON(r, v); err != nil {
			return err
		}
		if err := fillFromParams(r, v); err != nil {
			return err
		}
		return nil
	case strings.EqualFold(mediaType, "application/xml"),
		strings.EqualFold(mediaType, "text/xml"):
		if err := DecodeXML(r, v); err != nil {
			return err
		}
		if err := fillFromParams(r, v); err != nil {
			return err
		}
		return nil
	case strings.EqualFold(mediaType, "application/yaml"),
		strings.EqualFold(mediaType, "application/x-yaml"),
		strings.EqualFold(mediaType, "text/yaml"):
		if err := DecodeYAML(r, v); err != nil {
			return err
		}
		if err := fillFromParams(r, v); err != nil {
			return err
		}
		return nil
	case strings.EqualFold(mediaType, "application/x-www-form-urlencoded"),
		strings.EqualFold(mediaType, "multipart/form-data"):
		if err := parseForm(r, mediaType); err != nil {
			return err
		}
		if err := fillFromValuesAndParams(r, r.Form, v); err != nil {
			return err
		}
		if vv, ok := v.(util.Validator); ok {
			return vv.Validate()
		}
		return nil
	default:
		return types.UnsupportedMediaType(fmt.Sprintf("unsupported media type: %q", mediaType))
	}
}

// DecodeJSON decodes the request body as JSON. It only applies to requests
// whose Content-Type is application/json; otherwise it returns a 415 error.
// DecodeJSON 将请求体解码为 JSON，仅针对 Content-Type 为 application/json 的请求，
// 否则返回 415 错误。
func DecodeJSON(r *http.Request, v any) error {
	mediaType := mediaTypeOf(r)
	if !strings.EqualFold(mediaType, "application/json") {
		return types.UnsupportedMediaType(fmt.Sprintf("unsupported media type: %q", mediaType))
	}

	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("decode json: %w", err)
	}

	if vv, ok := v.(util.Validator); ok {
		return vv.Validate()
	}
	return nil
}

// DecodeXML decodes the request body as XML. It only applies to requests
// whose Content-Type is application/xml or text/xml; otherwise it returns a 415 error.
// DecodeXML 将请求体解码为 XML，仅针对 Content-Type 为 application/xml 或 text/xml
// 的请求，否则返回 415 错误。
func DecodeXML(r *http.Request, v any) error {
	mediaType := mediaTypeOf(r)
	if !strings.EqualFold(mediaType, "application/xml") &&
		!strings.EqualFold(mediaType, "text/xml") {
		return types.UnsupportedMediaType(fmt.Sprintf("unsupported media type: %q", mediaType))
	}

	defer r.Body.Close()
	if err := xml.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("decode xml: %w", err)
	}
	// 修复DecodeXML函数中Validator接口引用，应使用util.Validator
	if vv, ok := v.(util.Validator); ok {
		return vv.Validate()
	}
	return nil
}

// DecodeYAML decodes the request body as YAML. It only applies to requests
// whose Content-Type is application/yaml, application/x-yaml or text/yaml;
// otherwise it returns a 415 error.
// DecodeYAML 将请求体解码为 YAML，仅针对 Content-Type 为 application/yaml、
// application/x-yaml 或 text/yaml 的请求，否则返回 415 错误。
func DecodeYAML(r *http.Request, v any) error {
	mediaType := mediaTypeOf(r)
	if !strings.EqualFold(mediaType, "application/yaml") &&
		!strings.EqualFold(mediaType, "application/x-yaml") &&
		!strings.EqualFold(mediaType, "text/yaml") {
		return types.UnsupportedMediaType(fmt.Sprintf("unsupported media type: %q", mediaType))
	}

	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if err := yaml.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode yaml: %w", err)
	}

	if vv, ok := v.(util.Validator); ok {
		return vv.Validate()
	}
	return nil
}

// DecodeForm decodes the request body as form data. It only applies to requests
// whose Content-Type is application/x-www-form-urlencoded or multipart/form-data;
// otherwise it returns a 415 error. The target v can be a *url.Values,
// a *map[string][]string, or a pointer to a struct whose fields are filled by the
// `form` tag (falling back to the field name).
// DecodeForm 将请求体解码为表单数据，仅针对 Content-Type 为
// application/x-www-form-urlencoded 或 multipart/form-data 的请求，否则返回 415 错误。
// 目标 v 可以是 *url.Values、*map[string][]string，或指向结构体的指针，
// 结构体字段按 `form` tag（缺省时按字段名）填充。
func DecodeForm(r *http.Request, v any) error {
	mediaType := mediaTypeOf(r)
	if !strings.EqualFold(mediaType, "application/x-www-form-urlencoded") &&
		!strings.EqualFold(mediaType, "multipart/form-data") {
		return types.UnsupportedMediaType(fmt.Sprintf("unsupported media type: %q", mediaType))
	}

	if err := parseForm(r, mediaType); err != nil {
		return err
	}

	if err := fillFromValues(r.Form, v); err != nil {
		return err
	}

	if vv, ok := v.(util.Validator); ok {
		return vv.Validate()
	}
	return nil
}

// mediaTypeOf returns the media type of the request's Content-Type header.
// mediaTypeOf 返回请求 Content-Type 头的媒体类型。
func mediaTypeOf(r *http.Request) string {
	return strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
}

// parseForm parses the request body according to the media type.
// parseForm 根据媒体类型解析请求体。
func parseForm(r *http.Request, mediaType string) error {
	defer r.Body.Close()
	if strings.EqualFold(mediaType, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return fmt.Errorf("parse multipart form: %w", err)
		}
	} else if err := r.ParseForm(); err != nil {
		return fmt.Errorf("parse form: %w", err)
	}
	return nil
}

// fillFromValuesAndParams fills the target from parsed form values and URL
// parameters (path values and query string) in a single struct traversal.
// Struct fields use the `form` tag (falling back to the field name) for form
// data, and the `param` tag for URL parameters. When both a form value and a
// URL parameter are present for the same field, the URL parameter wins.
//
// fillFromValuesAndParams 在一次结构体遍历中同时从解析后的表单值和 URL 参数
// （路径值和查询字符串）填充目标。结构体字段使用 `form` tag（缺省为字段名）
// 匹配表单数据，使用 `param` tag 匹配 URL 参数。当同一字段同时存在表单值和
// URL 参数时，URL 参数优先。
func fillFromValuesAndParams(r *http.Request, values url.Values, v any) error {
	switch dst := v.(type) {
	case *url.Values:
		*dst = values
		return nil
	case *map[string][]string:
		*dst = map[string][]string(values)
		return nil
	}

	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("fill form+params: target must be a non-nil pointer")
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("fill form+params: target must point to a struct")
	}

	q := r.URL.Query()
	return fillStructBoth(rv, values, r, q)
}

// fillFromValues fills the target from the parsed form values.
// fillFromValues 将解析后的表单值填充到目标中。
func fillFromValues(values url.Values, v any) error {
	switch dst := v.(type) {
	case *url.Values:
		*dst = values
		return nil
	case *map[string][]string:
		*dst = map[string][]string(values)
		return nil
	}

	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("fill form: target must be a non-nil pointer")
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("fill form: target must point to a struct")
	}
	return fillStruct(rv, values)
}

// fillStructBoth traverses the struct once and fills fields from both form
// values (via `form` tag) and URL parameters (via `param` tag). URL parameters
// take precedence when both sources provide a value for the same field.
//
// fillStructBoth 一次遍历结构体，同时从表单值（`form` tag）和 URL 参数
// （`param` tag）填充字段。当同一字段两者都提供值时，URL 参数优先。
func fillStructBoth(rv reflect.Value, values url.Values, r *http.Request, q url.Values) error {
	rt := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		field := rv.Field(i)
		if !field.CanSet() {
			continue
		}
		ft := rt.Field(i)
		if ft.Anonymous {
			if err := fillStructBoth(field, values, r, q); err != nil {
				return err
			}
			continue
		}

		// Form tag — field name is used as fallback.
		// Form tag — 字段名作为缺省值。
		{
			name := ft.Tag.Get("form")
			if name == "" {
				name = ft.Name
			}
			if vals, ok := values[name]; ok && len(vals) > 0 {
				if err := setField(field, vals); err != nil {
					return fmt.Errorf("fill form field %q: %w", name, err)
				}
				continue
			}
		}

		// Param tag — path value takes precedence over query string.
		// Param tag — 路径参数优先于查询字符串。
		if name := ft.Tag.Get("param"); name != "" {
			val := r.PathValue(name)
			if val == "" {
				val = q.Get(name)
			}
			if val == "" {
				continue
			}
			if err := setField(field, []string{val}); err != nil {
				return fmt.Errorf("fill param field %q: %w", name, err)
			}
		}
	}
	return nil
}

func fillStruct(rv reflect.Value, values url.Values) error {
	rt := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		field := rv.Field(i)
		if !field.CanSet() {
			continue
		}
		ft := rt.Field(i)
		if ft.Anonymous {
			if err := fillStruct(field, values); err != nil {
				return err
			}
			continue
		}

		name := ft.Tag.Get("form")
		if name == "" {
			name = ft.Name
		}
		vals, ok := values[name]
		if !ok || len(vals) == 0 {
			continue
		}

		if err := setField(field, vals); err != nil {
			return fmt.Errorf("fill form field %q: %w", name, err)
		}
	}
	return nil
}

// setField sets the struct field from the form values.
// setField 用表单值设置结构体字段。
func setField(field reflect.Value, vals []string) error {
	if field.Kind() == reflect.Slice && field.Type().Elem().Kind() != reflect.Slice {
		slice := reflect.MakeSlice(field.Type(), len(vals), len(vals))
		for i, s := range vals {
			if err := setScalar(slice.Index(i), s); err != nil {
				return err
			}
		}
		field.Set(slice)
		return nil
	}
	return setScalar(field, vals[0])
}

// setScalar parses a single form value into a scalar field.
// setScalar 将单个表单值解析为标量字段。
func setScalar(field reflect.Value, s string) error {
	switch field.Kind() {
	case reflect.String:
		field.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("parse bool %q: %w", s, err)
		}
		field.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("parse int %q: %w", s, err)
		}
		field.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u, err := strconv.ParseUint(s, 10, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("parse uint %q: %w", s, err)
		}
		field.SetUint(u)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("parse float %q: %w", s, err)
		}
		field.SetFloat(f)
	default:
		return fmt.Errorf("unsupported field type %s", field.Type())
	}
	return nil
}

// fillFromParams fills struct fields tagged with `param` from URL path values
// and query parameters. For each field tagged with `param:"<name>"`, it first
// tries r.PathValue(name); if empty, it falls back to r.URL.Query().Get(name).
// If neither source provides a non-empty value, the field is skipped.
//
// fillFromParams 从 URL 路径值和查询参数中填充带 `param` tag 的结构体字段。
// 对于每个带有 `param:"<名称>"` tag 的字段，优先从 r.PathValue 获取；
// 若为空则回退到 r.URL.Query().Get。若两者均无值则跳过该字段。
func fillFromParams(r *http.Request, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("fill params: target must be a non-nil pointer")
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("fill params: target must point to a struct")
	}
	return fillStructFromParams(rv, r)
}

// fillStructFromParams recursively walks the struct and sets fields whose
// `param` tag matches a path value or query parameter.
//
// fillStructFromParams 递归遍历结构体，设置 `param` tag 匹配路径值或查询参数的字段。
func fillStructFromParams(rv reflect.Value, r *http.Request) error {
	rt := rv.Type()
	q := r.URL.Query()
	for i := 0; i < rv.NumField(); i++ {
		field := rv.Field(i)
		if !field.CanSet() {
			continue
		}
		ft := rt.Field(i)
		if ft.Anonymous {
			if err := fillStructFromParams(field, r); err != nil {
				return err
			}
			continue
		}

		name := ft.Tag.Get("param")
		if name == "" {
			continue
		}

		// Path value takes precedence; fall back to query string.
		// 路径参数优先，查询参数作为回退。
		val := r.PathValue(name)
		if val == "" {
			val = q.Get(name)
		}
		if val == "" {
			continue
		}

		if err := setField(field, []string{val}); err != nil {
			return fmt.Errorf("fill param field %q: %w", name, err)
		}
	}
	return nil
}
