// Package cli implements pgleash's command-line boundary.
package cli

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"
)

const help = `Usage:
  pgleash --profiles
  pgleash --help

Options:
  --profiles  List configured connection profiles (table format).
  --profile NAME  Validate a configured profile name; listing still includes all profiles.
  --format table  Select table output (the only format available yet).
  --help      Show this help without reading configuration.

SQL execution and JSON/CSV output are not available yet.
`

// Run executes the CLI and returns its process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	return run(args, stdout, stderr, currentIdentity)
}

type identity struct {
	home string
	uid  int
}

func currentIdentity() (identity, error) {
	uid := os.Geteuid()
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return identity{}, fmt.Errorf("cannot resolve executing user")
	}
	return identity{home: u.HomeDir, uid: uid}, nil
}

func run(args []string, stdout, stderr io.Writer, lookup func() (identity, error)) int {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "PG") {
			if err := os.Unsetenv(name); err != nil {
				fmt.Fprintln(stderr, "pgleash: cannot clear PG environment")
				return 1
			}
		}
	}
	opts, err := parseOptions(args)
	if err != nil {
		fmt.Fprintln(stderr, "pgleash:", err)
		return 1
	}
	if opts.help {
		if _, err := io.WriteString(stdout, help); err == nil {
			return 0
		}
		fmt.Fprintln(stderr, "pgleash: cannot write output")
		return 1
	}
	if opts.profiles {
		who, err := lookup()
		if err != nil {
			fmt.Fprintln(stderr, "pgleash: cannot resolve executing user")
			return 1
		}
		config, err := readConfig(who)
		if err != nil {
			fmt.Fprintln(stderr, "pgleash:", err)
			return 1
		}
		if opts.profile != "" {
			if _, ok := config.Profiles[opts.profile]; !ok {
				fmt.Fprintln(stderr, "pgleash: selected profile does not exist")
				return 1
			}
		}
		var table strings.Builder
		w := tabwriter.NewWriter(&table, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tPORT\tUSER\tDATABASE")
		names := make([]string, 0, len(config.Profiles))
		for name := range config.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			p := config.Profiles[name]
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", displayText(name), *p.Port, displayText(*p.User), displayText(*p.Database))
		}
		w.Flush()
		if _, err := io.WriteString(stdout, table.String()); err != nil {
			fmt.Fprintln(stderr, "pgleash: cannot write output")
			return 1
		}
		return 0
	}
	fmt.Fprintln(stderr, "pgleash: expected --profiles or --help")
	return 1
}

func displayText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			quoted := strconv.QuoteRune(r)
			b.WriteString(quoted[1 : len(quoted)-1])
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

type options struct {
	help     bool
	profiles bool
	profile  string
}

func parseOptions(args []string) (options, error) {
	var opts options
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		switch name {
		case "--help", "--profiles":
			if hasValue {
				return options{}, fmt.Errorf("help and profiles do not take values")
			}
			if name == "--help" {
				opts.help = true
			} else {
				opts.profiles = true
			}
		case "--profile", "--format":
			if !hasValue {
				i++
				if i == len(args) {
					return options{}, fmt.Errorf("option requires a value")
				}
				value = args[i]
			}
			if value == "" {
				return options{}, fmt.Errorf("option requires a nonempty value")
			}
			if name == "--profile" {
				opts.profile = value
			} else if value != "table" {
				return options{}, fmt.Errorf("only table format is available yet")
			}
		default:
			return options{}, fmt.Errorf("unrecognized option or unsupported invocation; use --help")
		}
	}
	if opts.help && len(args) == 1 || opts.profiles && !opts.help {
		return opts, nil
	}
	return options{}, fmt.Errorf("expected --profiles or --help")
}
