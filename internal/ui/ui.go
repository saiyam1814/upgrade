// Package ui contains the small set of UI primitives that every
// command shares: ANSI color, severity glyphs, confirmation prompts,
// and progress messages. We deliberately avoid heavy TUI deps here
// (bubbletea lives in internal/tui only) so headless / CI invocations
// stay lean.
//
// Color is auto-disabled when stdout is not a TTY or when NO_COLOR /
// FORCE_COLOR are set.
package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Style codes — kept tiny and dependency-free.
const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	dim     = "\033[2m"
	red     = "\033[31m"
	yellow  = "\033[33m"
	green   = "\033[32m"
	blue    = "\033[34m"
	magenta = "\033[35m"
	cyan    = "\033[36m"
	gray    = "\033[90m"
)

// Enabled reports whether to emit ANSI codes.
func Enabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// Colorize wraps s in style codes when color is enabled.
func Colorize(s, code string) string {
	if !Enabled() {
		return s
	}
	return code + s + reset
}

// Helpers — short names for terse call sites.
func Red(s string) string     { return Colorize(s, red) }
func Yellow(s string) string  { return Colorize(s, yellow) }
func Green(s string) string   { return Colorize(s, green) }
func Blue(s string) string    { return Colorize(s, blue) }
func Magenta(s string) string { return Colorize(s, magenta) }
func Cyan(s string) string    { return Colorize(s, cyan) }
func Bold(s string) string    { return Colorize(s, bold) }
func Dim(s string) string     { return Colorize(s, dim) }
func Gray(s string) string    { return Colorize(s, gray) }

// Banner prints a visually distinct title block.
func Banner(w io.Writer, title, subtitle string) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, Bold(Cyan("▎ "+title)))
	if subtitle != "" {
		fmt.Fprintln(w, Dim("  "+subtitle))
	}
	fmt.Fprintln(w)
}

// Step prints one numbered step in a runbook.
func Step(w io.Writer, n int, total int, title string) {
	fmt.Fprintf(w, "%s %s\n", Cyan(fmt.Sprintf("[%d/%d]", n, total)), Bold(title))
}

// SubStep prints a sub-bullet under the current step.
func SubStep(w io.Writer, glyph, text string) {
	fmt.Fprintf(w, "    %s %s\n", glyph, text)
}

// OK / Warn / Err — short status helpers.
func OK(w io.Writer, msg string)   { fmt.Fprintf(w, "%s %s\n", Green("✓"), msg) }
func Warn(w io.Writer, msg string) { fmt.Fprintf(w, "%s %s\n", Yellow("⚠"), msg) }
func Err(w io.Writer, msg string)  { fmt.Fprintf(w, "%s %s\n", Red("✗"), msg) }
func Info(w io.Writer, msg string) { fmt.Fprintf(w, "%s %s\n", Blue("ℹ"), msg) }

// Command renders an external command for the user to copy/paste.
func Command(w io.Writer, cmd string) {
	fmt.Fprintln(w, "    "+Magenta("$ "+cmd))
}

// Confirm prompts y/N. Returns true only on explicit "y" or "yes".
// Auto-cancels (returns false) when stdin is not a TTY — never silently
// proceeds in CI / pipelines.
func Confirm(prompt string) bool {
	fi, err := os.Stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
		fmt.Fprintln(os.Stderr, Yellow("⚠ refusing to prompt — stdin is not a TTY. Re-run with --yes to bypass."))
		return false
	}
	fmt.Fprintf(os.Stderr, "%s %s ", Bold("?"), prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// Hr prints a separator line.
func Hr(w io.Writer) {
	fmt.Fprintln(w, Dim(strings.Repeat("─", 64)))
}
