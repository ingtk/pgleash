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
  pgleash [options] "SQL"
  pgleash --profiles
  pgleash --help

Options:
  --profiles  List configured connection profiles (table format).
  --profile NAME  Select a configured connection profile.
  --format table  Select table output (the only format available yet).
  --timeout SECONDS  Execution deadline, 1-300 (default 30).
  --limit ROWS       Total displayed rows, 1-10000 (default 500).
  --max-width CHARS  Table cell width, 0-10000; 0 disables shortening (default 200).
  --help      Show this help without reading configuration.

JSON/CSV output is not available yet. SQL may contain multiple statements.
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
	{
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
		selected := opts.profile
		if selected == "" {
			selected = *config.DefaultProfile
		}
		if _, ok := config.Profiles[selected]; !ok {
			fmt.Fprintln(stderr, "pgleash: selected profile does not exist")
			return 1
		}
		if !opts.profiles {
			p := config.Profiles[selected]
			batch, err := executeSQL(p, opts)
			if err != nil {
				fmt.Fprintln(stderr, "pgleash:", sqlDiagnostic(err, *p.Password))
				return 1
			}
			output, err := renderResults(batch, opts)
			if err != nil {
				fmt.Fprintln(stderr, "pgleash:", sqlDiagnostic(&sqlFailure{err, true}, *p.Password))
				return 1
			}
			if _, err := io.WriteString(stdout, output); err != nil {
				fmt.Fprintln(stderr, "pgleash: cannot write output; database changes may already be committed; do not retry automatically")
				return 1
			}
			if batch.omitted {
				fmt.Fprintln(stderr, "pgleash: rows omitted by --limit")
			}
			return 0
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
	sql      string
	timeout  int
	limit    int
	maxWidth int
}

func parseOptions(args []string) (options, error) {
	opts := options{timeout: 30, limit: 500, maxWidth: 200}
	var positional []string
	endOptions := false
	for i := 0; i < len(args); i++ {
		if endOptions || strings.HasPrefix(args[i], "--") && strings.ContainsAny(args[i], "\r\n") {
			positional = append(positional, args[i])
			continue
		}
		if args[i] == "--" {
			endOptions = true
			continue
		}
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
		case "--profile", "--format", "--timeout", "--limit", "--max-width":
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
			} else if name == "--format" && value != "table" {
				return options{}, fmt.Errorf("only table format is available yet")
			} else if name != "--format" {
				n, err := strconv.Atoi(value)
				min, max := 1, 10000
				if name == "--timeout" {
					max = 300
				}
				if name == "--max-width" {
					min = 0
				}
				if err != nil || n < min || n > max {
					return options{}, fmt.Errorf("numeric option is outside its supported range")
				}
				switch name {
				case "--timeout":
					opts.timeout = n
				case "--limit":
					opts.limit = n
				case "--max-width":
					opts.maxWidth = n
				}
			}
		default:
			if strings.HasPrefix(args[i], "-") {
				return options{}, fmt.Errorf("unrecognized option; use --help")
			}
			positional = append(positional, args[i])
		}
	}
	if opts.help && len(args) == 1 || opts.profiles && !opts.help && len(positional) == 0 {
		return opts, nil
	}
	if opts.help || opts.profiles || len(positional) != 1 || strings.TrimSpace(positional[0]) == "" || strings.HasPrefix(strings.TrimSpace(positional[0]), `\`) {
		return options{}, fmt.Errorf("expected exactly one nonempty SQL argument, --profiles, or --help")
	}
	opts.sql = positional[0]
	return opts, nil
}
