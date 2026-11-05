package httpscript

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// jsonPath is the JSONPath subset httpscript supports: $, .name, ['name'],
// [n] and * or [*].
type jsonPath struct {
	steps []step
	multi bool // has a wildcard, so the result is a list
}

type step struct {
	name  string
	index int
	kind  byte // 'n' name, 'i' index, '*' wildcard
}

func parseJSONPath(s string) (*jsonPath, error) {
	if !strings.HasPrefix(s, "$") {
		return nil, fmt.Errorf("%q doesn't start with $", s)
	}
	p := &jsonPath{}
	unsupported := func(what string) error { return fmt.Errorf("%q: %s isn't supported", s, what) }
	for i := 1; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], ".."):
			return nil, unsupported("..")
		case strings.HasPrefix(s[i:], ".*"):
			p.steps, p.multi = append(p.steps, step{kind: '*'}), true
			i += 2
		case strings.HasPrefix(s[i:], "[*]"):
			p.steps, p.multi = append(p.steps, step{kind: '*'}), true
			i += 3
		case s[i] == '.':
			j := i + 1
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			if j == i+1 {
				return nil, fmt.Errorf("%q: empty name at %d", s, i)
			}
			p.steps = append(p.steps, step{kind: 'n', name: s[i+1 : j]})
			i = j
		case strings.HasPrefix(s[i:], "['"), strings.HasPrefix(s[i:], `["`):
			end := strings.Index(s[i+2:], string(s[i+1])+"]")
			if end < 0 {
				return nil, fmt.Errorf("%q: unterminated [ at %d", s, i)
			}
			p.steps = append(p.steps, step{kind: 'n', name: s[i+2 : i+2+end]})
			i += end + 4
		case s[i] == '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("%q: unterminated [ at %d", s, i)
			}
			n, err := strconv.Atoi(s[i+1 : i+end])
			if err != nil {
				return nil, unsupported(s[i : i+end+1])
			}
			p.steps = append(p.steps, step{kind: 'i', index: n})
			i += end + 1
		default:
			return nil, fmt.Errorf("%q: unexpected %q at %d", s, s[i], i)
		}
	}
	return p, nil
}

// eval returns what p selects in doc, decoded by encoding/json. A path with
// a wildcard always finds a list, possibly empty.
func (p *jsonPath) eval(doc any) (any, bool) {
	cur := []any{doc}
	for _, st := range p.steps {
		var next []any
		for _, v := range cur {
			switch st.kind {
			case 'n':
				if m, ok := v.(map[string]any); ok {
					if x, ok := m[st.name]; ok {
						next = append(next, x)
					}
				}
			case 'i':
				if a, ok := v.([]any); ok {
					i := st.index
					if i < 0 {
						i += len(a)
					}
					if i >= 0 && i < len(a) {
						next = append(next, a[i])
					}
				}
			case '*':
				switch v := v.(type) {
				case []any:
					next = append(next, v...)
				case map[string]any:
					keys := make([]string, 0, len(v))
					for k := range v {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					for _, k := range keys {
						next = append(next, v[k])
					}
				}
			}
		}
		cur = next
	}
	if p.multi {
		if cur == nil {
			cur = []any{}
		}
		return cur, true
	}
	if len(cur) == 0 {
		return nil, false
	}
	return cur[0], true
}
