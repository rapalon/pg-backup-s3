package pg

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a PostgreSQL major.minor version. Development builds such as
// "18beta1" or "17devel" parse with Minor 0.
type Version struct {
	Major int
	Minor int
}

func (v Version) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// Compare returns -1, 0 or 1 like strings.Compare.
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		if v.Major < o.Major {
			return -1
		}
		return 1
	case v.Minor != o.Minor:
		if v.Minor < o.Minor {
			return -1
		}
		return 1
	}
	return 0
}

var versionRE = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)

// ParseVersion extracts the first major[.minor] number from strings such as
// "pg_dump (PostgreSQL) 18.6", "16.15 (Debian 16.15-1)" or "17devel".
func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("no version number in %q", strings.TrimSpace(s))
	}
	major, _ := strconv.Atoi(m[1])
	minor := 0
	if m[2] != "" {
		minor, _ = strconv.Atoi(m[2])
	}
	if major == 0 {
		return Version{}, fmt.Errorf("invalid version in %q", strings.TrimSpace(s))
	}
	return Version{Major: major, Minor: minor}, nil
}

// ClientVersion returns the version of a client tool (`<binary> --version`),
// cached per binary for the lifetime of the Runner.
func (r *Runner) ClientVersion(ctx context.Context, binary string) (Version, error) {
	if v, ok := r.versions.Load(binary); ok {
		return v.(Version), nil
	}
	var out strings.Builder
	if err := r.Run(ctx, Spec{Binary: binary, Args: []string{"--version"}, Stdout: &out}); err != nil {
		return Version{}, err
	}
	v, err := ParseVersion(out.String())
	if err != nil {
		return Version{}, fmt.Errorf("%s --version: %w", binary, err)
	}
	r.versions.Store(binary, v)
	return v, nil
}

// ServerVersion asks the server for its version through psql.
func (r *Runner) ServerVersion(ctx context.Context, conn Conn) (Version, error) {
	rows, err := r.Query(ctx, conn, "show server_version")
	if err != nil {
		return Version{}, err
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return Version{}, fmt.Errorf("show server_version returned nothing")
	}
	v, err := ParseVersion(rows[0][0])
	if err != nil {
		return Version{}, fmt.Errorf("server_version: %w", err)
	}
	return v, nil
}
