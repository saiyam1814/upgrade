// Package tui is the interactive visual upgrade dashboard.
//
// Layout: a fixed-height shell using lipgloss.Place so the header,
// two side-by-side panes, and the status footer always fit the
// terminal exactly — no overflow into the next line, no orphaned
// "step done" messages floating below the borders.
//
// When the user presses `r`, we shell out to the headless command
// with --format json, parse the report, and render the parsed
// findings in the right pane. We never check just the exit code —
// that's why the previous TUI showed "no issues found" even when
// the underlying command had emitted blockers.
package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/saiyam1814/upgrade/internal/cloud"
	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/recommend"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
	"github.com/saiyam1814/upgrade/internal/sources/live"
)

// Run starts the TUI for a given target.
func Run(target string, kubeconfig, contextName string) error {
	tgt, ok := apis.Parse(target)
	if !ok && target != "" {
		return fmt.Errorf("invalid --target %q", target)
	}
	m := initialModel(tgt, kubeconfig, contextName)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// ---- model ----

type stepStatus int

const (
	statusPending stepStatus = iota
	statusRunning
	statusOK
	statusBlocked
	statusWarn
)

type step struct {
	Name        string
	Description string
	RunCmd      []string // shell command for "r" key; nil = no-op
	Status      stepStatus
	Findings    []finding.Finding
	Commands    []string // emitted commands (from cloud.Plan, etc.)
	RawOutput   string   // last raw command output (for debugging / non-JSON output)
	Recommend   string   // smart "next:" hint (from recommend.NextStep)
	LastRun     time.Time
}

type model struct {
	target       apis.Semver
	kubeconfig   string
	contextName  string
	clusterInfo  string
	provider     string
	steps        []step
	cursor       int
	width        int
	height       int
	statusLine   string
	clusterReady bool
	spin         spinner.Model
	viewport     viewport.Model
	vpReady      bool
}

type clusterReadyMsg struct {
	info     string
	provider string
	commands []string
}

type stepDoneMsg struct {
	idx       int
	output    string
	findings  []finding.Finding
	status    stepStatus
	recommend string
}

func initialModel(target apis.Semver, kubeconfig, contextName string) model {
	t := target.String()
	steps := []step{
		{Name: "Preflight", Description: "scan + simulate + addons + pdb + volumes + vcluster",
			RunCmd: []string{"kubectl-upgrade", "preflight", "--target", t, "--fail-on", "none", "--format", "json"}},
		{Name: "Plan", Description: "Cloud-CLI commands for control plane + nodes",
			RunCmd: []string{"kubectl-upgrade", "run", "plan", "--target", t}},
		{Name: "Backup", Description: "Velero / etcd snapshot reminder (manual step)"},
		{Name: "Control Plane", Description: "Run cloud-CLI command from Plan step yourself"},
		{Name: "Watch", Description: "Stuck-state monitor",
			RunCmd: []string{"kubectl-upgrade", "run", "watch", "--stop-after", "10"}},
		{Name: "Node Pools", Description: "Bump worker nodes (manual)"},
		{Name: "Verify", Description: "Server version + rescan",
			RunCmd: []string{"kubectl-upgrade", "run", "verify", "--target", t, "--fail-on", "none"}},
		{Name: "Fleet", Description: "vCluster Tenant Clusters wave",
			RunCmd: []string{"kubectl-upgrade", "fleet", "--host-target", t, "--plan"}},
	}
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("87"))

	return model{
		target:      target,
		kubeconfig:  kubeconfig,
		contextName: contextName,
		steps:       steps,
		statusLine:  "↑↓ navigate · enter run step · q quit",
		spin:        sp,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		detectCluster(m.kubeconfig, m.contextName, m.target),
		m.spin.Tick,
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Layout math (in terminal cells):
		//   header  = 1 row
		//   footer  = 1 row
		//   body    = m.height - 2 (the panes go here)
		//   pane border (top+bottom) = 2 rows
		//   pane inner content area = body - 2
		// viewport content must fit pane inner exactly, otherwise
		// content overflows past the bottom border.
		rightW, paneInnerH := m.paneSize()
		if !m.vpReady {
			m.viewport = viewport.New(rightW, paneInnerH)
			m.vpReady = true
		} else {
			m.viewport.Width = rightW
			m.viewport.Height = paneInnerH
		}
		m.viewport.SetContent(m.renderRight(m.steps[m.cursor]))

	case clusterReadyMsg:
		m.clusterInfo = msg.info
		m.provider = msg.provider
		m.steps[1].Commands = msg.commands
		m.clusterReady = true
		m.viewport.SetContent(m.renderRight(m.steps[m.cursor]))

	case stepDoneMsg:
		s := &m.steps[msg.idx]
		s.Status = msg.status
		s.Findings = msg.findings
		s.RawOutput = msg.output
		s.Recommend = msg.recommend
		s.LastRun = time.Now()
		m.statusLine = fmt.Sprintf("✓ ran %s · ↑↓ navigate · enter rerun · q quit", s.Name)
		if msg.idx == m.cursor {
			m.viewport.SetContent(m.renderRight(*s))
			m.viewport.GotoTop()
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
				m.viewport.SetContent(m.renderRight(m.steps[m.cursor]))
				m.viewport.GotoTop()
			}
		case "down", "j":
			if m.cursor < len(m.steps)-1 {
				m.cursor++
				m.viewport.SetContent(m.renderRight(m.steps[m.cursor]))
				m.viewport.GotoTop()
			}
		case "pgup":
			m.viewport.HalfPageUp()
		case "pgdown", " ":
			m.viewport.HalfPageDown()
		case "home", "g":
			m.viewport.GotoTop()
		case "end", "G":
			m.viewport.GotoBottom()
		case "r", "enter":
			s := &m.steps[m.cursor]
			if len(s.RunCmd) == 0 {
				m.statusLine = "this step is manual — execute the cloud-CLI commands shown yourself"
				return m, nil
			}
			s.Status = statusRunning
			m.statusLine = fmt.Sprintf("running: %s", s.Name)
			m.viewport.SetContent(m.renderRight(*s))
			return m, runStep(m.cursor, s.RunCmd)
		}
	}
	return m, nil
}

func (m model) View() string {
	if m.width == 0 || !m.vpReady {
		return "starting…"
	}
	rightW, paneInnerH := m.paneSize()
	bodyH := paneInnerH + 2 // outer pane height = inner + 2 border rows

	header := renderHeader(m)
	leftPane := renderLeft(m, bodyH)
	// MaxHeight forces lipgloss to truncate if the rendered pane would
	// exceed bodyH rows. Without this, an oversized viewport (or content
	// that lipgloss couldn't size correctly) pushes the layout past the
	// terminal bottom and the top of the screen scrolls off.
	rightPane := paneStyle.
		Width(rightW).
		Height(bodyH).
		MaxHeight(bodyH).
		Render(clipToHeight(m.viewport.View(), paneInnerH))

	body := lipgloss.JoinHorizontal(lipgloss.Top, leftPane, rightPane)
	footer := renderFooter(m)

	full := lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
	// Final defensive clip — no matter what View returned, never give
	// bubbletea more than (width × height) cells.
	return clipToHeight(lipgloss.Place(m.width, m.height, lipgloss.Left, lipgloss.Top, full), m.height)
}

// clipToHeight returns at most n lines of s. Belt-and-braces guard
// against any subtree that misreports its height.
func clipToHeight(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// paneSize returns the width of the right pane (after subtracting the
// left pane and inter-pane margin) AND the inner content height of any
// pane (after subtracting border rows).
func (m model) paneSize() (rightW, innerH int) {
	rightW = m.width - (m.width / 3) - 4
	if rightW < 20 {
		rightW = 20
	}
	// header (1) + footer (1) = 2 reserved rows for the chrome.
	bodyH := m.height - 2
	innerH = bodyH - 2 // top border + bottom border
	if innerH < 3 {
		innerH = 3
	}
	return rightW, innerH
}

func renderHeader(m model) string {
	tgt := m.target.String()
	if tgt == "v0.0" {
		tgt = "(none)"
	}
	left := headerStyle.Render(fmt.Sprintf("kubectl-upgrade · target %s", tgt))
	right := ""
	if m.clusterReady && m.clusterInfo != "" {
		right = dimStyle.Render(m.clusterInfo)
	}
	pad := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

func renderLeft(m model, bodyH int) string {
	var rows []string
	for i, s := range m.steps {
		marker := "  "
		if i == m.cursor {
			marker = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render("▎ ")
		}
		statusGlyph := statusGlyphFor(s.Status, m.spin)
		row := fmt.Sprintf("%s%s %s", marker, statusGlyph, s.Name)
		if i == m.cursor {
			row = selectedStyle.Render(row)
		}
		rows = append(rows, row)
	}
	left := lipgloss.JoinVertical(lipgloss.Left, rows...)
	return paneStyle.Width(m.width / 3).Height(bodyH).MaxHeight(bodyH).Render(left)
}

func (m model) renderRight(s step) string {
	var b strings.Builder

	b.WriteString(titleStyle.Render(s.Name))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(s.Description))
	b.WriteString("\n\n")

	if s.Status == statusRunning {
		b.WriteString(m.spin.View() + " running…\n")
		return b.String()
	}

	if !s.LastRun.IsZero() {
		b.WriteString(dimStyle.Render(fmt.Sprintf("last run: %s", s.LastRun.Format("15:04:05"))))
		b.WriteString("\n\n")
	} else if len(s.RunCmd) == 0 {
		b.WriteString(noteStyle.Render("This step is manual. Read the description above and run the commands yourself."))
		b.WriteString("\n\n")
	} else {
		b.WriteString(dimStyle.Render("press enter / r to run this step"))
		b.WriteString("\n")
	}

	// Commands the user should run (e.g. cloud-CLI commands from `run plan`).
	if len(s.Commands) > 0 {
		b.WriteString(sectionStyle.Render("Commands to run yourself"))
		b.WriteString("\n")
		for _, c := range s.Commands {
			if strings.TrimSpace(c) == "" {
				b.WriteString("\n")
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(c), "#") {
				b.WriteString("  " + dimStyle.Render(c) + "\n")
				continue
			}
			b.WriteString("  " + cmdStyle.Render("$ "+c) + "\n")
		}
		b.WriteString("\n")
	}

	// Findings.
	if len(s.Findings) > 0 {
		counts := finding.Counts(s.Findings)
		b.WriteString(sectionStyle.Render(fmt.Sprintf(
			"Findings: %s · %s · %s · %d LOW · %d INFO",
			redStyle.Render(fmt.Sprintf("%d BLOCKER", counts[finding.Blocker])),
			yellowStyle.Render(fmt.Sprintf("%d HIGH", counts[finding.High])),
			cyanStyle.Render(fmt.Sprintf("%d MEDIUM", counts[finding.Medium])),
			counts[finding.Low], counts[finding.Info],
		)))
		b.WriteString("\n\n")
		shown := 0
		for _, f := range s.Findings {
			if f.Severity == finding.Info || f.Severity == finding.Low {
				continue
			}
			if shown >= 20 {
				b.WriteString(dimStyle.Render(fmt.Sprintf("  … and %d more (run the headless command for the full list)", len(s.Findings)-shown)))
				b.WriteString("\n")
				break
			}
			shown++
			b.WriteString("  " + severityGlyph(f.Severity) + " " + f.Title + "\n")
			if f.Object != nil && f.Object.String() != "" {
				b.WriteString("      " + dimStyle.Render(f.Object.String()) + "\n")
			}
			if f.Fix != "" {
				b.WriteString("      " + dimStyle.Render("fix: "+truncateOneLine(f.Fix, 200)) + "\n")
			}
		}
	} else if !s.LastRun.IsZero() && s.Status == statusOK {
		b.WriteString(okStyle.Render("✓ no upgrade-blocking issues found"))
		b.WriteString("\n")
	} else if !s.LastRun.IsZero() && s.Status == statusBlocked {
		b.WriteString(redStyle.Render("✗ command failed"))
		b.WriteString("\n")
		if s.RawOutput != "" {
			b.WriteString("\n")
			b.WriteString(dimStyle.Render("Last output (last 30 lines):"))
			b.WriteString("\n")
			b.WriteString(tailLines(s.RawOutput, 30))
			b.WriteString("\n")
		}
	}

	// Smart "next:" recommendation.
	if s.Recommend != "" {
		b.WriteString("\n")
		b.WriteString(sectionStyle.Render("→ Next"))
		b.WriteString("\n")
		b.WriteString("  " + s.Recommend + "\n")
	}

	return b.String()
}

func renderFooter(m model) string {
	help := "↑↓ navigate · enter run · pgup/pgdn scroll · q quit"
	left := footerStyle.Render(help)
	right := ""
	if m.statusLine != "" {
		right = footerStyle.Foreground(lipgloss.Color("220")).Render(m.statusLine)
	}
	pad := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

// ---- styles ----

var (
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("87")).Padding(0, 1)
	footerStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Padding(0, 1)
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("87"))
	sectionStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("219"))
	cmdStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	redStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	yellowStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	cyanStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("87"))
	noteStyle     = lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("214"))
	paneStyle     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("237")).Padding(0, 1).MarginRight(1)
)

func statusGlyphFor(s stepStatus, sp spinner.Model) string {
	switch s {
	case statusOK:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("82")).Render("✓")
	case statusBlocked:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("✗")
	case statusWarn:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("⚠")
	case statusRunning:
		return sp.View()
	}
	return dimStyle.Render("○")
}

func severityGlyph(s finding.Severity) string {
	switch s {
	case finding.Blocker:
		return redStyle.Render("✗")
	case finding.High:
		return yellowStyle.Render("⚠")
	case finding.Medium:
		return cyanStyle.Render("•")
	}
	return dimStyle.Render("·")
}

func truncateOneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// ---- commands ----

func detectCluster(kubeconfig, contextName string, target apis.Semver) tea.Cmd {
	return func() tea.Msg {
		c, err := live.Connect(kubeconfig, contextName)
		if err != nil {
			return clusterReadyMsg{info: "(no cluster: " + err.Error() + ")"}
		}
		cl, err := cloud.Detect(context.Background(), c.Core)
		if err != nil {
			return clusterReadyMsg{info: "(detect failed)"}
		}
		plan := cl.Plan(target.String())
		var cmds []string
		cmds = append(cmds, plan.PreReqs...)
		cmds = append(cmds, plan.ControlPlane...)
		cmds = append(cmds, plan.NodePools...)
		return clusterReadyMsg{
			info:     fmt.Sprintf("provider=%s · server=%s · nodes=%d", cl.Provider, cl.GitVersion, cl.NodeCount),
			provider: string(cl.Provider),
			commands: cmds,
		}
	}
}

// jsonReport mirrors the shape that internal/report writes when
// --format json is used. We intentionally redefine it here (with
// just the fields we need) rather than importing internal/report,
// to keep the TUI a thin renderer over the headless contract.
type jsonReport struct {
	Header struct {
		Tool   string `json:"tool"`
		Source string `json:"source"`
		Target string `json:"target"`
	} `json:"header"`
	Counts   map[string]int    `json:"counts"`
	Findings []finding.Finding `json:"findings"`
}

func runStep(idx int, args []string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		// Run the headless command. If --format json was added by
		// initialModel, we'll parse the stdout as JSON; otherwise we
		// keep the raw output for display.
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()

		out := stdout.String()
		if out == "" {
			out = stderr.String()
		}

		// Try to parse as JSON. If it parses, use the structured findings.
		var status stepStatus = statusOK
		var findings []finding.Finding
		var rec string
		if hasFlag(args, "--format", "json") {
			var rep jsonReport
			if jerr := json.Unmarshal(stdout.Bytes(), &rep); jerr == nil {
				findings = rep.Findings
				if rep.Counts["BLOCKER"] > 0 {
					status = statusBlocked
				} else if rep.Counts["HIGH"] > 0 {
					status = statusWarn
				}
				// Build a recommend.Context to get the smart "next:".
				rec = recommend.NextStep(recommend.Context{
					Command:  cobraCommandName(args),
					Target:   rep.Header.Target,
					Findings: findings,
				})
			} else if err == nil {
				// Command succeeded but JSON didn't parse — treat as OK with raw output.
				status = statusOK
			} else {
				status = statusBlocked
			}
		} else {
			// Non-JSON commands (run plan, run watch, fleet --plan): exit
			// code is the only signal we have.
			if err != nil {
				status = statusBlocked
			}
		}

		return stepDoneMsg{
			idx:       idx,
			output:    out,
			findings:  findings,
			status:    status,
			recommend: rec,
		}
	}
}

// hasFlag returns true if args contains "--<name>" with the given value (or "--<name>=<value>").
func hasFlag(args []string, name, value string) bool {
	for i, a := range args {
		if a == name && i+1 < len(args) && args[i+1] == value {
			return true
		}
		if a == name+"="+value {
			return true
		}
	}
	return false
}

// cobraCommandName extracts the command name (e.g. "preflight", "run plan",
// "fleet") from the args slice for the recommendation engine.
func cobraCommandName(args []string) string {
	if len(args) < 2 {
		return ""
	}
	// args[0] is "kubectl-upgrade"; args[1..] is the cobra path.
	switch args[1] {
	case "run":
		if len(args) >= 3 {
			return "run " + args[2]
		}
	case "fleet":
		if len(args) >= 3 && args[2] == "drift" {
			return "fleet drift"
		}
		return "fleet"
	}
	return args[1]
}
