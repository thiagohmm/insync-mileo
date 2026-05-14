package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/pkg/browser"
	"github.com/thiagohmm/insync-clone/api/proto/insync"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var docStyle = lipgloss.NewStyle().Margin(1, 2)

type item struct {
	title, desc string
}

func (i item) Title() string       { return i.title }
func (i item) Description() string { return i.desc }
func (i item) FilterValue() string { return i.title }

// browseItem representa uma linha na navegação pós-auth; Path no proto = ID remoto no Drive.
type browseItem struct {
	title       string
	desc        string
	remoteID    string
	isDirectory bool
	isBack      bool
}

func (i browseItem) Title() string       { return i.title }
func (i browseItem) Description() string { return i.desc }
func (i browseItem) FilterValue() string { return i.title }

type statusMsg *insync.SyncStatusResponse

type fileListMsg struct {
	files []*insync.FileInfo
}

type listErrMsg struct {
	text string
}

// pollAuthMsg dispara checagem periódica AddAccount(código vazio) após abrir o OAuth no navegador.
type pollAuthMsg struct{}

type state int

const (
	stateProviders state = iota
	stateAuthCode
	stateBrowsing
)

type model struct {
	list       list.Model
	progress   progress.Model
	status     string
	client     insync.InsyncServiceClient
	ctx        context.Context
	cancel     context.CancelFunc
	state      state
	accountID  string
	provider   insync.Provider
	authCode   string
	browsePath []string // do root ao diretório atual, ex.: {"root", "abc..."}
}

func providerItems() []list.Item {
	return []list.Item{
		item{title: "Google Drive", desc: "Sincronização ativa"},
		item{title: "OneDrive", desc: "Aguardando configuração"},
	}
}

func initialModel(client insync.InsyncServiceClient) model {
	m := model{
		list:     list.New(providerItems(), list.NewDefaultDelegate(), 0, 0),
		progress: progress.New(progress.WithDefaultGradient()),
		status:   "Conectado ao Daemon",
		client:   client,
		state:    stateProviders,
	}
	m.list.Title = "Insync Clone - Provedores"
	m.list.SetFilteringEnabled(false)
	m.list.SetShowFilter(false)
	m.list.DisableQuitKeybindings()
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m
}

func (m model) Init() tea.Cmd {
	// Não inscreve em GetSyncStatus aqui: o stream polui a tela de OAuth e a auth é por callback no servidor.
	return nil
}

func pollAuthTick() tea.Cmd {
	return tea.Tick(750*time.Millisecond, func(time.Time) tea.Msg {
		return pollAuthMsg{}
	})
}

func (m model) afterAuthSuccess() (model, tea.Cmd) {
	m.state = stateBrowsing
	m.browsePath = []string{"root"}
	m.list.Title = "Google Drive — b: base-sync | s: full-sync"
	m.status = "Autenticado! Carregando lista da nuvem..."
	return m, tea.Batch(m.fetchFiles("root"), m.waitForStatus())
}

func (m model) waitForStatus() tea.Cmd {
	return func() tea.Msg {
		stream, err := m.client.GetSyncStatus(m.ctx, &insync.SyncStatusRequest{})
		if err != nil {
			return nil
		}
		res, err := stream.Recv()
		if err != nil {
			return nil
		}
		return statusMsg(res)
	}
}

func (m model) fetchFiles(folderID string) tea.Cmd {
	return func() tea.Msg {
		res, err := m.client.ListFiles(m.ctx, &insync.ListFilesRequest{
			FolderPath: folderID,
			AccountId:  m.accountID,
		})
		if err != nil {
			return listErrMsg{text: err.Error()}
		}
		return fileListMsg{files: res.Files}
	}
}

func (m model) configureSyncForSelection(mode insync.SyncMode) (model, tea.Cmd) {
	sel, ok := m.list.SelectedItem().(browseItem)
	if !ok || sel.isBack {
		return m, nil
	}
	if !sel.isDirectory {
		m.status = "Escolha uma pasta para sincronizar (não um arquivo)."
		return m, nil
	}
	suffix := safeLocalDir(sel.title)
	var local string
	if mode == insync.SyncMode_BASE_SYNC {
		local = "./sync-base-" + suffix
	} else {
		local = "./sync-full-" + suffix
	}
	modeLabel := "base-sync"
	if mode == insync.SyncMode_FULL_SYNC {
		modeLabel = "full-sync"
	}
	_, err := m.client.ConfigureSync(m.ctx, &insync.ConfigureSyncRequest{
		AccountId:      m.accountID,
		LocalPath:      local,
		RemoteFolderId: sel.remoteID,
		Mode:           mode,
	})
	if err != nil {
		m.status = "Erro ao configurar sync: " + err.Error()
	} else {
		m.status = fmt.Sprintf("Sync (%s): nuvem → %s — Base: apagar local não remove na nuvem; apagar na nuvem remove aqui. Full: espelha tudo.", modeLabel, local)
	}
	return m, nil
}

func safeLocalDir(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r == '/' || r == '\\' || r == ':' || r < 32 {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
	}
	s := strings.TrimSpace(b.String())
	if s == "" {
		return "sync-folder"
	}
	return s
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+c" {
			m.cancel()
			return m, tea.Quit
		}
		// Em browsing, a lista precisa receber ↑/↓/j/k/pgup/etc. antes de qualquer outra lógica.
		// Ex.: msg.String() nem sempre cobre todos os casos; passar ao componente bubbles resolve o foco.
		if m.state == stateBrowsing && key != "backspace" && key != "enter" && key != "s" && key != "b" {
			var cmd tea.Cmd
			m.list, cmd = m.list.Update(msg)
			return m, cmd
		}

		switch key {
		case "backspace":
			if m.state == stateBrowsing || m.state == stateAuthCode {
				m.state = stateProviders
				m.list.Title = "Insync Clone - Provedores"
				m.authCode = ""
				m.browsePath = nil
				m.list.SetItems(providerItems())
				return m, nil
			}
		case "b":
			if m.state == stateBrowsing {
				return m.configureSyncForSelection(insync.SyncMode_BASE_SYNC)
			}
		case "s":
			if m.state == stateBrowsing {
				return m.configureSyncForSelection(insync.SyncMode_FULL_SYNC)
			}
		case "enter":
			if m.state == stateProviders {
				i, ok := m.list.SelectedItem().(item)
				if ok {
					if i.title == "Google Drive" {
						m.provider = insync.Provider_GOOGLE_DRIVE
					} else {
						m.provider = insync.Provider_ONEDRIVE
					}

					res, err := m.client.GetAuthURL(m.ctx, &insync.GetAuthURLRequest{Provider: m.provider})
					if err == nil {
						_ = browser.OpenURL(res.Url)
						m.state = stateAuthCode
						m.authCode = ""
						m.status = "Autorize no navegador. Quando voltar a esta tela, a lista abre sozinha; ou cole o código e Enter."
						return m, pollAuthTick()
					}
					m.status = "Erro ao obter URL de autenticação"
				}
			} else if m.state == stateAuthCode {
				res, err := m.client.AddAccount(m.ctx, &insync.AddAccountRequest{
					Provider: m.provider,
					AuthCode: m.authCode,
				})
				if err == nil && res.Success {
					m.accountID = res.AccountId
					return m.afterAuthSuccess()
				}
				m.status = "Erro na autenticação. Verifique o código ou tente de novo no navegador."
			} else if m.state == stateBrowsing {
				sel, ok := m.list.SelectedItem().(browseItem)
				if !ok {
					return m, nil
				}
				if sel.isBack {
					if len(m.browsePath) <= 1 {
						return m, nil
					}
					m.browsePath = m.browsePath[:len(m.browsePath)-1]
					parent := m.browsePath[len(m.browsePath)-1]
					m.status = "Carregando..."
					return m, m.fetchFiles(parent)
				}
				if sel.isDirectory {
					m.browsePath = append(m.browsePath, sel.remoteID)
					m.status = "Carregando..."
					return m, m.fetchFiles(sel.remoteID)
				}
				m.status = "É um arquivo — escolha uma pasta e use b ou s."
			}
		default:
			if m.state == stateAuthCode && len(key) == 1 {
				m.authCode += key
				return m, nil
			}
		}
	case fileListMsg:
		var items []list.Item
		if len(m.browsePath) > 1 {
			items = append(items, browseItem{title: "..", desc: "Voltar", isBack: true})
		}
		for _, f := range msg.files {
			desc := "Arquivo"
			if f.IsDirectory {
				desc = "Pasta — Enter abrir | b base-sync | s full-sync"
			}
			items = append(items, browseItem{
				title:       f.Name,
				desc:        desc,
				remoteID:    f.Path,
				isDirectory: f.IsDirectory,
			})
		}
		m.list.SetItems(items)
		m.list.ResetSelected()
		cur := "root"
		if len(m.browsePath) > 0 {
			cur = m.browsePath[len(m.browsePath)-1]
		}
		m.status = fmt.Sprintf("Pasta: %s — ↑↓/jk mover | Enter abrir | b base-sync | s full-sync | .. voltar", cur)
		return m, nil
	case pollAuthMsg:
		if m.state != stateAuthCode {
			return m, nil
		}
		res, err := m.client.AddAccount(m.ctx, &insync.AddAccountRequest{
			Provider: m.provider,
			AuthCode: "",
		})
		if err == nil && res.Success && res.AccountId != "" {
			m.accountID = res.AccountId
			return m.afterAuthSuccess()
		}
		return m, pollAuthTick()
	case listErrMsg:
		m.status = "Erro ao listar: " + msg.text
		return m, nil
	case tea.WindowSizeMsg:
		h, v := docStyle.GetFrameSize()
		m.list.SetSize(msg.Width-h, msg.Height-v-5)
	case statusMsg:
		if m.state != stateBrowsing {
			return m, nil
		}
		m.status = fmt.Sprintf("Syncing: %s", msg.FilePath)
		cmd := m.progress.SetPercent(float64(msg.ProgressPercentage) / 100.0)
		return m, tea.Batch(cmd, m.waitForStatus())
	case progress.FrameMsg:
		newModel, cmd := m.progress.Update(msg)
		if pm, ok := newModel.(progress.Model); ok {
			m.progress = pm
		}
		return m, cmd
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) View() string {
	var s string
	if m.state == stateAuthCode {
		s = docStyle.Render(fmt.Sprintf("Autenticação %s\n\n%s\n\nCódigo: %s", m.provider, m.status, m.authCode))
	} else {
		s = docStyle.Render(m.list.View())
		s += "\n\n" + m.status + "\n"
	}
	s += "\n" + m.progress.View() + "\n\n"
	s += "ctrl+c sair | backspace provedores | b base-sync | s full-sync\n"
	return s
}

const serverAddr = "localhost:50051"

// serverRunning checks whether a process accepts TCP connections on serverAddr.
func serverRunning() bool {
	conn, err := net.DialTimeout("tcp", serverAddr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// findServerBinary resolves the path to the insync-server binary.
// It first tries alongside the CLI executable, then falls back to the cwd.
func findServerBinary() string {
	// Attempt to resolve relative to the running CLI binary
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "insync-server")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	// Fallback: current working directory
	if abs, err := filepath.Abs("insync-server"); err == nil {
		if _, statErr := os.Stat(abs); statErr == nil {
			return abs
		}
	}
	return "insync-server"
}

// startServer launches a detached insync-server process that survives when the CLI exits.
func startServer() *exec.Cmd {
	binary := findServerBinary()
	cmd := exec.Command(binary)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // detach from CLI process group
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start server (%s): %v", binary, err)
	}
	fmt.Println("Server started (background).")
	return cmd
}

// waitForServer blocks until serverRunning() returns true or a timeout is reached.
func waitForServer() {
	start := time.Now()
	for time.Since(start) < 10*time.Second {
		if serverRunning() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Fatal("timed out waiting for the server to start")
}

func main() {
	// Auto-start the server if it's not already running.
	if !serverRunning() {
		startServer()
		waitForServer()
	}

	conn, err := grpc.Dial("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()
	client := insync.NewInsyncServiceClient(conn)

	p := tea.NewProgram(initialModel(client), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Alas, there's been an error: %v", err)
		os.Exit(1)
	}
}
