package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
)

var validate = validator.New(validator.WithRequiredStructEnabled())

func init() {
	validate.RegisterTagNameFunc(func(f reflect.StructField) string {
		for _, tag := range []string{"json", "query", "path"} {
			if n, _, _ := strings.Cut(f.Tag.Get(tag), ","); n != "" && n != "-" {
				return n
			}
		}
		return f.Name
	})
}

func Validator() *validator.Validate { return validate }

type bindOptions struct {
	maxBytes     int64
	allowUnknown bool
	skipBody     bool
	skipValidate bool
}

type BindOption func(*bindOptions)

func MaxBodyBytes(n int64) BindOption { return func(o *bindOptions) { o.maxBytes = n } }

func AllowUnknownFields() BindOption { return func(o *bindOptions) { o.allowUnknown = true } }

func NoBody() BindOption { return func(o *bindOptions) { o.skipBody = true } }

func SkipValidation() BindOption { return func(o *bindOptions) { o.skipValidate = true } }

func Bind[T any](r *http.Request, opts ...BindOption) (T, error) {
	o := bindOptions{maxBytes: 10 << 20}
	for _, opt := range opts {
		opt(&o)
	}
	var v T
	rv := reflect.ValueOf(&v).Elem()
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			rv.Set(reflect.New(rv.Type().Elem()))
		}
		rv = rv.Elem()
	}

	hasBody := r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0
	if !o.skipBody && hasBody {
		if err := decodeBody(r, &v, o); err != nil {
			return v, err
		}
	}
	if rv.Kind() == reflect.Struct {
		if err := bindTagged(rv, "query", func(k string) ([]string, bool) {
			vals, ok := r.URL.Query()[k]
			return vals, ok
		}); err != nil {
			return v, err
		}
		if err := bindTagged(rv, "path", func(k string) ([]string, bool) {
			s := r.PathValue(k)
			return []string{s}, s != ""
		}); err != nil {
			return v, err
		}
		if !o.skipValidate {
			if err := validate.Struct(rv.Interface()); err != nil {
				return v, validationError(err)
			}
		}
	}
	return v, nil
}

func decodeBody(r *http.Request, into any, o bindOptions) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || (mt != "application/json" && !strings.HasSuffix(mt, "+json")) {
			return E(CodeMediaType)
		}
	}
	body := http.MaxBytesReader(nil, r.Body, o.maxBytes)
	dec := json.NewDecoder(body)
	if !o.allowUnknown {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(into); err != nil {
		var mbe *http.MaxBytesError
		var ute *json.UnmarshalTypeError
		switch {
		case errors.As(err, &mbe):
			return E(CodeTooLarge).Wrap(err)
		case errors.Is(err, io.EOF):
			return nil
		case errors.As(err, &ute):
			return E(CodeValidation).WithFields(FieldError{Field: ute.Field, Rule: "type", Message: "must be " + ute.Type.String()}).Wrap(err)
		case strings.HasPrefix(err.Error(), "json: unknown field"):
			field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
			return E(CodeValidation).WithFields(FieldError{Field: field, Rule: "unknown", Message: "is not allowed"}).Wrap(err)
		}
		return E(CodeInvalidJSON).Wrap(err)
	}
	if dec.More() {
		return E(CodeInvalidJSON).WithMessage("The request body must contain a single JSON value")
	}
	return nil
}

func bindTagged(rv reflect.Value, tag string, get func(string) ([]string, bool)) error {
	t := rv.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := rv.Field(i)
		if f.Anonymous && fv.Kind() == reflect.Struct {
			if err := bindTagged(fv, tag, get); err != nil {
				return err
			}
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get(tag), ",")
		if name == "" || name == "-" {
			continue
		}
		vals, ok := get(name)
		if !ok || len(vals) == 0 {
			continue
		}
		if err := setValue(fv, vals); err != nil {
			return E(CodeValidation).WithFields(FieldError{Field: name, Rule: "type", Message: err.Error()}).Wrap(err)
		}
	}
	return nil
}

var durationType = reflect.TypeOf(time.Duration(0))
var timeType = reflect.TypeOf(time.Time{})

func setValue(fv reflect.Value, vals []string) error {
	if fv.Kind() == reflect.Pointer {
		n := reflect.New(fv.Type().Elem())
		if err := setValue(n.Elem(), vals); err != nil {
			return err
		}
		fv.Set(n)
		return nil
	}
	if fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() != reflect.Uint8 {
		var parts []string
		for _, v := range vals {
			parts = append(parts, strings.Split(v, ",")...)
		}
		s := reflect.MakeSlice(fv.Type(), len(parts), len(parts))
		for i, p := range parts {
			if err := setValue(s.Index(i), []string{strings.TrimSpace(p)}); err != nil {
				return err
			}
		}
		fv.Set(s)
		return nil
	}
	s := vals[0]
	switch {
	case fv.Type() == durationType:
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("must be a duration")
		}
		fv.SetInt(int64(d))
		return nil
	case fv.Type() == timeType:
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return fmt.Errorf("must be an RFC 3339 time")
		}
		fv.Set(reflect.ValueOf(t))
		return nil
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("must be true or false")
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("must be an integer")
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("must be a positive integer")
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(s, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("must be a number")
		}
		fv.SetFloat(n)
	default:
		return fmt.Errorf("unsupported field type %s", fv.Type())
	}
	return nil
}

func Validate(v any) error {
	if err := validate.Struct(v); err != nil {
		return validationError(err)
	}
	return nil
}

func validationError(err error) error {
	var ves validator.ValidationErrors
	if !errors.As(err, &ves) {
		return E(CodeValidation).Wrap(err)
	}
	fields := make([]FieldError, 0, len(ves))
	for _, fe := range ves {
		fields = append(fields, FieldError{Field: fieldPath(fe), Rule: fe.Tag(), Message: ruleMessage(fe)})
	}
	return E(CodeValidation).WithFields(fields...)
}

func fieldPath(fe validator.FieldError) string {
	ns := fe.Namespace()
	if _, rest, ok := strings.Cut(ns, "."); ok {
		return rest
	}
	return fe.Field()
}

func ruleMessage(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "is required"
	case "email":
		return "must be an email address"
	case "min":
		return "must be at least " + fe.Param()
	case "max":
		return "must be at most " + fe.Param()
	case "oneof":
		return "must be one of: " + fe.Param()
	case "uuid", "uuid4", "uuid7":
		return "must be a UUID"
	case "url":
		return "must be a URL"
	}
	return "failed rule " + fe.Tag()
}
