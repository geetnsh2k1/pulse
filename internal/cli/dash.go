package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"pulse/internal/engine"
	"pulse/internal/logs"
	"pulse/internal/store"
	"pulse/internal/ui"
)

// pulse ui — the live dashboard: functions, queue depths, streaming logs,
// and replayable event history, all in one full-screen terminal view.

var monitorCmd = &cobra.Command{
	Use:     "monitor",
	Aliases: []string{"ui", "dash"},
	Short:   "Live dashboard — logs, queues, and replayable history in one screen",
	Long: `A full-screen view of your running project: every function, live queue
depths, the streaming log feed (press / to filter), and recent events —
arrow onto one and press Enter to replay it.

Needs a running engine (` + "`pulse start`" + ` in another terminal).`,
	Args: cobra.NoArgs,
	RunE: runDash,
}

func init() {
	monitorCmd.ValidArgsFunction = cobra.NoFileCompletions
	rootCmd.AddCommand(monitorCmd)
}

func runDash(_ *cobra.Command, _ []string) error {
	if !stdinIsInteractive() {
		return fmt.Errorf("the dashboard needs a terminal")
	}
	cfg, err := loadProject()
	if err != nil {
		return err
	}
	info, running := engine.Current(cfg.Root)
	if !running {
		return fmt.Errorf("the dashboard shows a live engine — run `pulse start` in another terminal first")
	}

	m := newDashModel(cfg.Project, info)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout))

	// Logs arrive over SSE, pushed into the program from outside.
	go streamLogsInto(p, info)

	_, err = p.Run()
	return err
}

// ---- data types ----

type fnRow struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	ok, bad int
}

type qRow struct {
	Name     string `json:"name"`
	Visible  int    `json:"visible"`
	InFlight int    `json:"inFlight"`
	Delayed  int    `json:"delayed"`
}

type invRow struct {
	Function string `json:"function"`
	Status   string `json:"status"`
}

type dashModel struct {
	project string
	info    *engine.RunInfo

	width, height int
	focusEvents   bool // false: logs scroll · true: events select

	functions []fnRow
	queues    []qRow
	events    []store.EventRow
	selected  int

	logLines  []string // pre-rendered, ring-buffered
	logScroll int      // 0 = pinned to newest
	filter    string
	filtering bool

	toast   string
	dataErr string
}

func newDashModel(project string, info *engine.RunInfo) *dashModel {
	return &dashModel{project: project, info: info, width: 100, height: 30}
}

// ---- messages ----

type tickMsg struct{}
type logMsg string
type streamDown struct{}
type refreshMsg struct {
	functions []fnRow
	queues    []qRow
	events    []store.EventRow
	err       string
}
type replayedMsg struct {
	id, status string
	ms         int64
	err        string
}

func (m *dashModel) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

// refreshCmd polls the control API for everything except logs.
func (m *dashModel) refreshCmd() tea.Cmd {
	addr := m.info.Addr
	return func() tea.Msg {
		out := refreshMsg{}
		if err := getJSON(addr+"/api/functions", &out.functions); err != nil {
			out.err = err.Error()
			return out
		}
		_ = getJSON(addr+"/api/queues", &out.queues)
		_ = getJSON(addr+"/api/events?limit=8", &out.events)
		var invs []invRow
		_ = getJSON(addr+"/api/invocations?limit=200", &invs)
		counts := map[string]*fnRow{}
		for i := range out.functions {
			counts[out.functions[i].Name] = &out.functions[i]
		}
		for _, inv := range invs {
			if row, ok := counts[inv.Function]; ok {
				if inv.Status == "success" {
					row.ok++
				} else if inv.Status != "" {
					row.bad++
				}
			}
		}
		return out
	}
}

func (m *dashModel) replayCmd(id string) tea.Cmd {
	addr := m.info.Addr
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]string{"id": id})
		resp, err := http.Post(addr+"/api/replay", "application/json", bytes.NewReader(body))
		if err != nil {
			return replayedMsg{err: err.Error()}
		}
		defer resp.Body.Close()
		var out struct {
			Status     string `json:"status"`
			DurationMs int64  `json:"durationMs"`
			Error      string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if out.Error != "" && out.Status == "" {
			return replayedMsg{err: out.Error}
		}
		return replayedMsg{id: id, status: out.Status, ms: out.DurationMs}
	}
}

// streamLogsInto feeds SSE log lines into the program until it exits.
func streamLogsInto(p *tea.Program, info *engine.RunInfo) {
	resp, err := http.Get(info.Addr + "/api/logs/stream")
	if err != nil {
		p.Send(streamDown{})
		return
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var l logs.Line
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &l) == nil {
			p.Send(logMsg(renderLogLine(l)))
		}
	}
	p.Send(streamDown{})
}

func renderLogLine(l logs.Line) string {
	ts := time.UnixMilli(l.TS).Format("15:04:05")
	switch l.Stream {
	case "system":
		return fmt.Sprintf("%s %s", ui.Dim(ts), styleEventLine(l.Text))
	case "stderr":
		return fmt.Sprintf("%s %s %s %s", ui.Dim(ts), ui.Fn(l.Function), ui.Err("!"), l.Text)
	default:
		return fmt.Sprintf("%s %s %s %s", ui.Dim(ts), ui.Fn(l.Function), ui.Dim("|"), l.Text)
	}
}

func getJSON(url string, into any) error {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(into)
}

// ---- update ----

func (m *dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.refreshCmd(), tickCmd())

	case refreshMsg:
		m.dataErr = msg.err
		if msg.err == "" {
			m.functions, m.queues, m.events = msg.functions, msg.queues, msg.events
			if m.selected >= len(m.events) {
				m.selected = max(0, len(m.events)-1)
			}
		}
		return m, nil

	case logMsg:
		m.logLines = append(m.logLines, string(msg))
		if len(m.logLines) > 500 {
			m.logLines = m.logLines[len(m.logLines)-500:]
		}
		return m, nil

	case streamDown:
		m.toast = ui.Err("log stream closed — engine stopped?")
		return m, nil

	case replayedMsg:
		if msg.err != "" {
			m.toast = ui.Err("replay failed: " + msg.err)
		} else if msg.status == "success" {
			m.toast = ui.OK(fmt.Sprintf("↻ replayed %s → success · %dms", shortEventID(msg.id), msg.ms))
		} else {
			m.toast = ui.Warn(fmt.Sprintf("↻ replayed %s → %s", shortEventID(msg.id), msg.status))
		}
		return m, m.refreshCmd()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *dashModel) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filtering {
		switch k.Type {
		case tea.KeyEnter, tea.KeyEsc:
			m.filtering = false
			if k.Type == tea.KeyEsc {
				m.filter = ""
			}
		case tea.KeyBackspace:
			if len(m.filter) > 0 {
				m.filter = m.filter[:len(m.filter)-1]
			}
		case tea.KeyRunes:
			m.filter += string(k.Runes)
		}
		return m, nil
	}

	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		m.focusEvents = !m.focusEvents
	case "/":
		m.filtering = true
		m.focusEvents = false
	case "esc":
		m.filter = ""
	case "up", "k":
		if m.focusEvents {
			m.selected = max(0, m.selected-1)
		} else {
			m.logScroll++
		}
	case "down", "j":
		if m.focusEvents {
			m.selected = min(len(m.events)-1, m.selected+1)
		} else {
			m.logScroll = max(0, m.logScroll-1)
		}
	case "enter", "r":
		if m.focusEvents && m.selected < len(m.events) {
			ev := m.events[m.selected]
			m.toast = ui.Dim("replaying " + shortEventID(ev.ID) + "…")
			return m, m.replayCmd(ev.ID)
		}
	}
	return m, nil
}
