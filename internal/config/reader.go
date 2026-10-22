package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// reader pulls typed values out of the environment, applying the defaults
// from specs and collecting every problem instead of stopping at the first.
type reader struct {
	getenv   func(string) string
	readFile func(string) ([]byte, error)
	errs     []error
	seen     map[string]bool
}

func (r *reader) errorf(name, format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf("%s: %s", name, fmt.Sprintf(format, args...)))
}

func (r *reader) spec(name string) Spec {
	s, ok := specIndex[name]
	if !ok {
		panic("config: variable " + name + " is not in specs")
	}
	r.seen[name] = true
	return s
}

// raw returns the trimmed value of name, or the spec default when unset or empty.
func (r *reader) raw(name string) string {
	s := r.spec(name)
	if v := strings.TrimSpace(r.getenv(name)); v != "" {
		return v
	}
	return s.Default
}

func (r *reader) str(name string) string { return r.raw(name) }

// secret reads NAME or NAME_FILE. Setting both is an error; the file's
// trailing newline is trimmed so `echo secret > file` works.
func (r *reader) secret(name string) string {
	s := r.spec(name)
	if !s.AcceptsFile() {
		panic("config: " + name + " is not marked Secret or File")
	}
	direct := r.getenv(name)
	path := strings.TrimSpace(r.getenv(name + "_FILE"))
	switch {
	case direct != "" && path != "":
		r.errorf(name, "set either %s or %s_FILE, not both", name, name)
		return ""
	case path != "":
		b, err := r.readFile(path)
		if err != nil {
			r.errorf(name+"_FILE", "%v", err)
			return ""
		}
		return strings.TrimRight(string(b), "\r\n")
	default:
		return direct
	}
}

func (r *reader) oneOf(name string, allowed ...string) string {
	v := r.raw(name)
	for _, a := range allowed {
		if strings.EqualFold(v, a) {
			return a
		}
	}
	r.errorf(name, "%q is not one of %s", v, strings.Join(allowed, ", "))
	return allowed[0]
}

func (r *reader) boolean(name string) bool {
	v := r.raw(name)
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.errorf(name, "%q is not a boolean (true/false)", v)
		return false
	}
	return b
}

func (r *reader) integer(name string, min, max int) int {
	v := r.raw(name)
	n, err := strconv.Atoi(v)
	if err != nil {
		r.errorf(name, "%q is not an integer", v)
		return min
	}
	if n < min || n > max {
		r.errorf(name, "%d is outside %d..%d", n, min, max)
		return min
	}
	return n
}

func (r *reader) duration(name string, min time.Duration) time.Duration {
	v := r.raw(name)
	d, err := time.ParseDuration(v)
	if err != nil {
		r.errorf(name, "%q is not a duration (e.g. 30s, 5m, 6h)", v)
		return min
	}
	if d < min {
		r.errorf(name, "%s is below the minimum %s", d, min)
		return min
	}
	return d
}

func (r *reader) size(name string, min, max int64) int64 {
	v := r.raw(name)
	n, err := parseSize(v)
	if err != nil {
		r.errorf(name, "%v", err)
		return min
	}
	if n < min || n > max {
		r.errorf(name, "%s is outside %s..%s", v, formatSize(min), formatSize(max))
		return min
	}
	return n
}

// list splits a comma-separated value, trimming and dropping empty items.
func (r *reader) list(name string) []string {
	v := r.raw(name)
	if v == "" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func (r *reader) err() error {
	if len(r.errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n  %w", ErrInvalid, errors.Join(r.errs...))
}

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
	{"B", 1},
}

// parseSize parses "33554432", "32MiB", "64MB", "1.5GiB" or "5m" into bytes.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	upper := strings.ToUpper(s)
	for _, u := range sizeUnits {
		if !strings.HasSuffix(upper, u.suffix) {
			continue
		}
		num := strings.TrimSpace(s[:len(s)-len(u.suffix)])
		f, err := strconv.ParseFloat(num, 64)
		if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, fmt.Errorf("%q is not a size (e.g. 32MiB, 64MB, 33554432)", s)
		}
		return int64(f * float64(u.mult)), nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a size (e.g. 32MiB, 64MB, 33554432)", s)
	}
	return n, nil
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", n>>10)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
