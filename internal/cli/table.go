package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/jackc/pgx/v5/pgconn"
)

func renderResults(batch sqlBatch, opts options) (string, error) {
	var output boundedOutput
	for i, result := range batch.results {
		if i > 0 {
			output.WriteByte('\n')
		}
		fmt.Fprintf(&output, "Result %d\n", i+1)
		if len(result.fields) > 0 {
			w := tabwriter.NewWriter(&output, 0, 4, 2, ' ', 0)
			for j, field := range result.fields {
				if j > 0 {
					fmt.Fprint(w, "\t")
				}
				fmt.Fprint(w, tableCell(string(field.Name), opts.maxWidth))
			}
			fmt.Fprintln(w)
			for _, row := range result.rows {
				for j, value := range row {
					if j > 0 {
						fmt.Fprint(w, "\t")
					}
					if value.null {
						fmt.Fprint(w, "NULL")
					} else {
						fmt.Fprint(w, tableCell(value.text, opts.maxWidth))
					}
				}
				fmt.Fprintln(w)
			}
			if err := w.Flush(); err != nil {
				return "", err
			}
		}
		fmt.Fprintf(&output, "command: %s; rows_affected: %d\n", displayText(result.tag), pgconn.NewCommandTag(result.tag).RowsAffected())
	}
	if output.err != nil {
		return "", output.err
	}
	return output.String(), nil
}

type boundedOutput struct {
	buffer strings.Builder
	err    error
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if len(p) > resultCapacity-b.buffer.Len() {
		b.err = errResultCapacity
		return 0, b.err
	}
	return b.buffer.Write(p)
}
func (b *boundedOutput) WriteByte(c byte) error { _, err := b.Write([]byte{c}); return err }
func (b *boundedOutput) String() string         { return b.buffer.String() }

func tableCell(value string, width int) string {
	text := displayText(value)
	if width == 0 {
		return text
	}
	count := 0
	end := len(text)
	for index := range text {
		if count == width-1 {
			end = index
		}
		if count == width {
			return text[:end] + "…"
		}
		count++
	}
	return text
}
