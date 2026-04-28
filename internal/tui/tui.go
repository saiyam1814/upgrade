// Package tui is the interactive visual upgrade dashboard. It uses
// bubbletea + lipgloss to render an "ing-switch"-style migration
// path: a list of steps on the left, details/findings/commands on
// the right, color-coded by status.
//
// The TUI is read-only by default — running a step from the TUI
// shells out to the headless equivalent (kubectl-upgrade preflight,
// run plan, etc.) and streams its output. Mutating actions still
// require explicit --execute on the corresponding subcommand.
package tui

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/saiyam1814/upgrade/internal/cloud"
	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
	"github.com/saiyam1814/upgrade/internal/sources/live"
)

// Run starts the TUI for a given target. Returns when the user quits.
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
	LastRun     time.Time
}

type model struct {
	target       apis.Semver
	kubeconfig   string
	contextName  string
	clusterInfo  string
	steps        []step
	cursor       int
	width        int
	height       int
	statusLine   string
	clusterReady bool
}

type clusterReadyMsg struct {
	info     string
	commands []string
}

type stepDoneMsg struct {
	idx      int
	output   string
	findings []finding.Finding
	status   stepStatus
}

func initialModel(target apis.Semver, kubeconfig, contextName string) model {
	steps := []step{
		{Name: "Preflight", Description: "scan + simulate + addons + pdb + volumes + vcluster", RunCmd: []string{"kubectl-upgrade", "preflight", "--target", target.String(), "--fail-on", "none"}, Status: statusPending},
		{Name: "Plan", Description: "Cloud-CLI commands for control plane + nodes", RunCmd: []string{"kubectl-upgrade", "run", "plan", "--target", target.String()}, Status: statusPending},
		{Name: "Backup", Description: "Velero / etcd snapshot reminder", Status: statusPending},
		{Name: "Control Plane", Description: "Run cloud-CLI command (manual)", Status: statusPending},
		{Name: "Watch", Description: "Stuck-state monitor", RunCmd: []string{"kubectl-upgrade", "run", "watch", "--stop-after", "10"}, Status: statusPending},
		{Name: "Node Pools", Description: "Bump worker nodes (manual)", Status: statusPending},
		{Name: "Verify", Description: "Server version + rescan", RunCmd: []string{"kubectl-upgrade", "run", "verify", "--target", target.String(), "--fail-on", "none"}, Status: statusPending},
		{Name: "Fleet", Description: "vCluster Tenant Clusters wave", RunCmd: []string{"kubectl-upgrade", "fleet", "--host-target", target.String(), "--plan"}, Status: statusPending},
	}
	return model{
		target:      target,
		kubeconfig:  kubeconfig,
		contextName: contextName,
		steps:       steps,
		statusLine:  "press ↑↓ to navigate · r to run step · q to quit",
	}
}

func (m model) Init() tea.Cmd {
	return detectCluster(m.kubeconfig, m.contextName, m.target)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case clusterReadyMsg:
		m.clusterInfo = msg.info
		m.steps[1].Commands = msg.commands
		m.clusterReady = true
	case stepDoneMsg:
		s := &m.steps[msg.idx]
		s.Status = msg.status
		s.Findings = msg.findings
		s.LastRun = time.Now()
		m.statusLine = fmt.Sprintf("step %q done", s.Name)
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.steps)-1 {
				m.cursor++
			}
		case "r", "enter":
			s := &m.steps[m.cursor]
			if len(s.RunCmd) == 0 {
				m.statusLine = "this step is manual — run the emitted commands yourself"
				return m, nil
			}
			s.Status = statusRunning
			m.statusLine = "running: " + strings.Join(s.RunCmd, " ")
			return m, runStep(m.cursor, s.RunCmd)
		}
	}
	return m, nil
}

func (m model) View() string {
	if m.width == 0 {
		return "starting…"
	}

	header := headerStyle.Render(fmt.Sprintf("kubectl-upgrade · target %s", m.target))
	if m.clusterReady && m.clusterInfo != "" {
		header += "  " + dimStyle.Render(m.clusterInfo)
	}

	// Left pane — step list
	var leftLines []string
	for i, s := range m.steps {
		marker := "  "
		if i == m.cursor {
			marker = "▎ "
		}
		statusGlyph := statusGlyphFor(s.Status)
		row := fmt.Sprintf("%s%s %s", marker, statusGlyph, s.Name)
		if i == m.cursor {
			row = selectedStyle.Render(row)
		}
		leftLines = append(leftLines, row)
		if i == m.cursor {
			leftLines = append(leftLines, dimStyle.Render("    "+s.Description))
		}
	}
	left := lipgloss.JoinVertical(lipgloss.Left, leftLines...)

	// Right pane — details
	right := m.renderRight(m.steps[m.cursor])

	leftPane := paneStyle.Width(m.width / 3).Render(left)
	rightPane := paneStyle.Width(m.width - (m.width / 3) - 4).Render(right)

	body := lipgloss.JoinHorizontal(lipgloss.Top, leftPane, rightPane)

	footer := footerStyle.Render(m.statusLine)

	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m model) renderRight(s step) string {
	lines := []string{
		titleStyle.Render(s.Name),
		dimStyle.Render(s.Description),
		"",
	}
	if !s.LastRun.IsZero() {
		lines = append(lines, dimStyle.Render("last run: "+s.LastRun.Format("15:04:05")))
		lines = append(lines, "")
	}
	if len(s.Commands) > 0 {
		lines = append(lines, sectionStyle.Render("Commands to run yourself:"))
		for _, c := range s.Commands {
			lines = append(lines, cmdStyle.Render("  $ "+c))
		}
		lines = append(lines, "")
	}
	if len(s.Findings) > 0 {
		counts := finding.Counts(s.Findings)
		lines = append(lines, sectionStyle.Render(fmt.Sprintf("Findings: %d BLOCKER · %d HIGH · %d MEDIUM",
			counts[finding.Blocker], counts[finding.High], counts[finding.Medium])))
		for _, f := range s.Findings {
			if f.Severity != finding.Blocker && f.Severity != finding.High {
				continue
			}
			lines = append(lines, "  "+severityGlyph(f.Severity)+" "+f.Title)
		}
	} else if !s.LastRun.IsZero() {
		lines = append(lines, okStyle.Render("✓ no issues found"))
	} else {
		lines = append(lines, dimStyle.Render("press r to run this step"))
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

// ---- styles ----

var (
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("87")).Padding(0, 1).MarginBottom(1)
	footerStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).MarginTop(1)
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("87"))
	sectionStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("219"))
	cmdStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	paneStyle     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).MarginRight(1)
)

func statusGlyphFor(s stepStatus) string {
	switch s {
	case statusOK:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("82")).Render("✓")
	case statusBlocked:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("✗")
	case statusWarn:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("⚠")
	case statusRunning:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("87")).Render("↻")
	}
	return dimStyle.Render("○")
}

func severityGlyph(s finding.Severity) string {
	switch s {
	case finding.Blocker:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("✗")
	case finding.High:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("⚠")
	}
	return "·"
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
			commands: cmds,
		}
	}
}

func runStep(idx int, args []string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		status := statusOK
		if err != nil {
			status = statusBlocked
		}
		return stepDoneMsg{
			idx:    idx,
			output: string(out),
			status: status,
		}
	}
}
