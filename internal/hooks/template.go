package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The template language is deliberately tiny: literal text plus {{ expr }}
// where expr is a path into the JSON payload and an optional filter chain:
//
//	{{ .release.tag_name }}
//	{{ .repository.full_name | lower }}
//	{{ .ref | base | trimv }}
//	{{ .packages.0.version }}
//
// A path starts with a dot and continues with object keys or array indexes
// separated by dots (keys may use letters, digits, '_' and '-'). There are no
// functions, conditionals, loops or variables: nothing in a payload or template
// can execute code, read files or reach beyond the decoded document.
const maxTemplateOutput = 1024

// ErrTemplate is wrapped by every template error.
var ErrTemplate = errors.New("template error")

type segment struct {
	text  string
	expr  []string // path keys
	pipes []string // filters
}

func parseTemplate(tpl string) ([]segment, error) {
	if len(tpl) > 1024 {
		return nil, fmt.Errorf("%w: template too long", ErrTemplate)
	}
	var out []segment
	rest := tpl
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			if rest != "" {
				out = append(out, segment{text: rest})
			}
			return out, nil
		}
		if i > 0 {
			out = append(out, segment{text: rest[:i]})
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return nil, fmt.Errorf("%w: unterminated {{", ErrTemplate)
		}
		inner := strings.TrimSpace(rest[i+2 : i+j])
		rest = rest[i+j+2:]
		parts := strings.Split(inner, "|")
		path := strings.TrimSpace(parts[0])
		if !strings.HasPrefix(path, ".") || len(path) < 2 {
			return nil, fmt.Errorf("%w: expression must start with a dot path such as .release.tag_name", ErrTemplate)
		}
		keys := strings.Split(path[1:], ".")
		for _, k := range keys {
			if k == "" || len(k) > 128 {
				return nil, fmt.Errorf("%w: empty path segment", ErrTemplate)
			}
			for _, r := range k {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
					return nil, fmt.Errorf("%w: unsupported character in path", ErrTemplate)
				}
			}
		}
		seg := segment{expr: keys}
		for _, f := range parts[1:] {
			f = strings.TrimSpace(f)
			switch f {
			case "lower", "upper", "trimv", "base":
				seg.pipes = append(seg.pipes, f)
			default:
				return nil, fmt.Errorf("%w: unknown filter %q (lower, upper, trimv, base)", ErrTemplate, f)
			}
		}
		out = append(out, seg)
	}
}

// CheckTemplate validates the syntax of a template without evaluating it.
func CheckTemplate(tpl string) error {
	_, err := parseTemplate(tpl)
	return err
}

// TemplatePaths lists the payload paths a template reads (for sample payloads).
func TemplatePaths(tpl string) [][]string {
	segs, err := parseTemplate(tpl)
	if err != nil {
		return nil
	}
	var out [][]string
	for _, s := range segs {
		if s.expr != nil {
			out = append(out, s.expr)
		}
	}
	return out
}

// Render evaluates a template against a decoded JSON document. A path that is
// missing, null, an object or an array is an error, so an action never runs
// with an empty or structural value in place of a name.
func Render(tpl string, root any) (string, error) {
	segs, err := parseTemplate(tpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	for _, s := range segs {
		if s.expr == nil {
			b.WriteString(s.text)
			continue
		}
		v, ok := lookup(root, s.expr)
		if !ok {
			return "", fmt.Errorf("%w: .%s is not in the payload", ErrTemplate, strings.Join(s.expr, "."))
		}
		str, ok := scalar(v)
		if !ok {
			return "", fmt.Errorf("%w: .%s is not a scalar value", ErrTemplate, strings.Join(s.expr, "."))
		}
		for _, f := range s.pipes {
			switch f {
			case "lower":
				str = strings.ToLower(str)
			case "upper":
				str = strings.ToUpper(str)
			case "trimv":
				if len(str) > 1 && (str[0] == 'v' || str[0] == 'V') && str[1] >= '0' && str[1] <= '9' {
					str = str[1:]
				}
			case "base":
				if i := strings.LastIndex(str, "/"); i >= 0 {
					str = str[i+1:]
				}
			}
		}
		b.WriteString(str)
		if b.Len() > maxTemplateOutput {
			return "", fmt.Errorf("%w: result too long", ErrTemplate)
		}
	}
	return b.String(), nil
}

func lookup(root any, keys []string) (any, bool) {
	cur := root
	for _, k := range keys {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[k]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

func scalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, x != ""
	case json.Number:
		return x.String(), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

// decodePayload decodes a JSON body for templating. Non-JSON bodies yield nil
// (templates then fail with a clear error; non-templated actions still run).
func decodePayload(body []byte) any {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil
	}
	return v
}
