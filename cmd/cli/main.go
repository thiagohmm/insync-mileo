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
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/textinput"
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
	mode        insync.SyncMode
	hasMode     bool
}

func (i browseItem) Title() string {
	if i.isBack {
		return i.title
	}
	marker := "[]"
	if i.hasMode {
		if i.mode == insync.SyncMode_FULL_SYNC {
			marker = "[F]"
		} else {
			marker = "[B]"
		}
	}
	return marker + " " + i.title
}
func (i browseItem) Description() string { return i.desc }
func (i browseItem) FilterValue() string { return i.title }

type statusMsg *insync.SyncStatusResponse

type fileListMsg struct {
	files []*insync.FileInfo
}

type syncedListMsg struct {
	modes      map[string]insync.SyncMode
	localRoot  string
	configured bool
	count      int
}

type listErrMsg struct {
	text string
}

type syncStartedMsg struct {
	modes map[string]insync.SyncMode
	count int
	errs  []string
}

// pollAuthMsg dispara checagem periódica AddAccount(código vazio) após abrir o OAuth no navegador.
type pollAuthMsg struct{}

type state int

const (
	stateProviders state = iota
	stateAuthCode
	stateBrowsing
	stateLocalPath
)

type model struct {
	list          list.Model
	progress      progress.Model
	pathInput     textinput.Model
	status        string
	client        insync.InsyncServiceClient
	ctx           context.Context
	cancel        context.CancelFunc
	state         state
	accountID     string
	provider      insync.Provider
	authCode      string
	browsePath    []string // do root ao diretório atual, ex.: {"root", "abc..."}
	syncedModes   map[string]insync.SyncMode
	selectedItems map[string]browseItem
	lastLocalRoot string
}

func providerItems() []list.Item {
	return []list.Item{
		item{title: "Google Drive", desc: "Sincronização ativa"},
		item{title: "OneDrive", desc: "Aguardando configuração"},
	}
}

func initialModel(client insync.InsyncServiceClient) model {
	ti := textinput.New()
	ti.Placeholder = defaultSyncRoot()
	ti.Prompt = "Caminho local: "
	ti.CharLimit = 512
	ti.Width = 80
	m := model{
		list:          list.New(providerItems(), list.NewDefaultDelegate(), 0, 0),
		progress:      progress.New(progress.WithDefaultGradient()),
		pathInput:     ti,
		status:        "Conectado ao Daemon",
		client:        client,
		state:         stateProviders,
		syncedModes:   make(map[string]insync.SyncMode),
		selectedItems: make(map[string]browseItem),
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
	m.list.Title = "Google Drive — b: base-sync | f: full-sync"
	m.status = "Autenticado! Carregando lista da nuvem..."
	return m, tea.Batch(m.fetchFiles("root"), m.fetchSynced(), m.waitForStatus())
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

func (m model) fetchSynced() tea.Cmd {
	return func() tea.Msg {
		res, err := m.client.ListSyncedFiles(m.ctx, &insync.ListSyncedFilesRequest{
			AccountId: m.accountID,
		})
		if err != nil {
			return listErrMsg{text: err.Error()}
		}
		modes := make(map[string]insync.SyncMode, len(res.Files))
		localRoot := ""
		for _, f := range res.Files {
			modes[f.Path] = f.Mode
			if localRoot == "" && f.LocalPath != "" {
				candidate := filepath.Dir(f.LocalPath)
				if candidate != "." {
					localRoot = candidate
				}
			}
		}
		return syncedListMsg{modes: modes, localRoot: localRoot}
	}
}

func (m model) toggleSyncForSelection(mode insync.SyncMode) (model, tea.Cmd) {
	sel, ok := m.list.SelectedItem().(browseItem)
	if !ok || sel.isBack {
		return m, nil
	}
	sel.mode = mode
	sel.hasMode = true
	m.selectedItems[sel.remoteID] = sel
	m.syncedModes[sel.remoteID] = mode
	m.refreshVisibleMarkers()
	if m.lastLocalRoot != "" {
		if err := os.MkdirAll(m.lastLocalRoot, 0755); err != nil {
			m.status = "Erro ao criar caminho local: " + err.Error()
			return m, nil
		}
		m.status = fmt.Sprintf("Enviando %d item(ns) ao servidor...", len(m.selectedItems))
		return m, m.configureSelectedItems(m.lastLocalRoot)
	}
	m.status = fmt.Sprintf("%d item(ns) selecionado(s). Pressione p para escolher o caminho local.", len(m.selectedItems))
	return m, nil
}

func (m *model) refreshVisibleMarkers() {
	items := m.list.Items()
	next := make([]list.Item, 0, len(items))
	for _, it := range items {
		bi, ok := it.(browseItem)
		if !ok || bi.isBack {
			next = append(next, it)
			continue
		}
		if mode, ok := m.selectedItems[bi.remoteID]; ok {
			bi.mode = mode.mode
			bi.hasMode = true
		} else if mode, ok := m.syncedModes[bi.remoteID]; ok {
			bi.mode = mode
			bi.hasMode = true
		} else {
			bi.hasMode = false
		}
		next = append(next, bi)
	}
	m.list.SetItems(next)
}

func (m model) configureSelectedItems(localRoot string) tea.Cmd {
	items := make([]browseItem, 0, len(m.selectedItems))
	for _, item := range m.selectedItems {
		items = append(items, item)
	}
	return func() tea.Msg {
		var wg sync.WaitGroup
		var mu sync.Mutex
		errs := make([]string, 0)
		modes := make(map[string]insync.SyncMode, len(items))

		for idx, item := range items {
			idx := idx
			item := item
			wg.Add(1)
			go func() {
				defer wg.Done()
				localPath := filepath.Join(localRoot, safeLocalName(item.title))
				ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
				res, err := m.client.ConfigureSync(ctx, &insync.ConfigureSyncRequest{
					AccountId:      m.accountID,
					LocalPath:      localPath,
					RemoteFolderId: item.remoteID,
					Mode:           item.mode,
					IsDirectory:    item.isDirectory,
					DisplayName:    item.title,
				})
				cancel()
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, fmt.Sprintf("configurar %s (%d/%d): %v", item.title, idx+1, len(items), err))
					return
				}
				if !res.Success {
					errs = append(errs, fmt.Sprintf("configurar %s (%d/%d): %s", item.title, idx+1, len(items), res.ErrorMessage))
					return
				}
				modes[item.remoteID] = item.mode
			}()
		}
		wg.Wait()
		return syncStartedMsg{modes: modes, count: len(modes), errs: errs}
	}
}

func safeLocalName(name string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_")
	s := strings.TrimSpace(replacer.Replace(name))
	if s == "" || s == "." || s == ".." {
		return "sync-item"
	}
	return s
}

func defaultSyncRoot() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "Insync")
	}
	return "./Insync"
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+c" {
			m.cancel()
			return m, tea.Quit
		}
		if m.state == stateLocalPath {
			switch key {
			case "enter":
				localRoot := strings.TrimSpace(m.pathInput.Value())
				if localRoot == "" {
					localRoot = defaultSyncRoot()
				}
				if err := os.MkdirAll(localRoot, 0755); err != nil {
					m.status = "Erro ao criar caminho local: " + err.Error()
					return m, nil
				}
				m.lastLocalRoot = localRoot
				m.state = stateBrowsing
				m.status = fmt.Sprintf("Enviando %d item(ns) ao servidor...", len(m.selectedItems))
				return m, m.configureSelectedItems(localRoot)
			case "esc":
				m.state = stateBrowsing
				m.status = "Seleção mantida. Pressione p quando quiser escolher o caminho local."
				return m, nil
			}
			var cmd tea.Cmd
			m.pathInput, cmd = m.pathInput.Update(msg)
			return m, cmd
		}
		// Em browsing, a lista precisa receber ↑/↓/j/k/pgup/etc. antes de qualquer outra lógica.
		// Ex.: msg.String() nem sempre cobre todos os casos; passar ao componente bubbles resolve o foco.
		if m.state == stateBrowsing && key != "backspace" && key != "enter" && key != "f" && key != "b" && key != "p" {
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
				return m.toggleSyncForSelection(insync.SyncMode_BASE_SYNC)
			}
		case "f":
			if m.state == stateBrowsing {
				return m.toggleSyncForSelection(insync.SyncMode_FULL_SYNC)
			}
		case "p":
			if m.state == stateBrowsing {
				if len(m.selectedItems) == 0 {
					m.status = "Selecione ao menos um arquivo ou pasta com b ou f."
					return m, nil
				}
				m.state = stateLocalPath
				if m.lastLocalRoot != "" {
					m.pathInput.SetValue(m.lastLocalRoot)
				} else {
					m.pathInput.SetValue(defaultSyncRoot())
				}
				m.pathInput.Focus()
				m.status = "Informe a pasta onde os itens selecionados serão sincronizados."
				return m, textinput.Blink
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
				m.status = "Arquivo selecionável com b ou f. Pressione p para escolher o destino local."
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
				desc = "Pasta — Enter abrir | b base-sync | f full-sync"
			} else {
				desc = "Arquivo — b base-sync | f full-sync"
			}
			bi := browseItem{
				title:       f.Name,
				desc:        desc,
				remoteID:    f.Path,
				isDirectory: f.IsDirectory,
			}
			if selected, ok := m.selectedItems[f.Path]; ok {
				bi.mode = selected.mode
				bi.hasMode = true
			} else if mode, ok := m.syncedModes[f.Path]; ok {
				bi.mode = mode
				bi.hasMode = true
			}
			items = append(items, bi)
		}
		m.list.SetItems(items)
		m.list.ResetSelected()
		cur := "root"
		if len(m.browsePath) > 0 {
			cur = m.browsePath[len(m.browsePath)-1]
		}
		m.status = fmt.Sprintf("Pasta: %s — ↑↓/jk mover | Enter abrir pasta | b base-sync | f full-sync | p caminho local", cur)
		return m, nil
	case syncedListMsg:
		if msg.modes != nil {
			m.syncedModes = msg.modes
		}
		if msg.localRoot != "" {
			m.lastLocalRoot = msg.localRoot
		}
		if msg.configured {
			for id, item := range m.selectedItems {
				m.syncedModes[id] = item.mode
			}
			m.selectedItems = make(map[string]browseItem)
		}
		m.refreshVisibleMarkers()
		if msg.configured {
			m.status = fmt.Sprintf("%d item(ns) configurado(s). O servidor iniciou o sync em background.", msg.count)
		}
		return m, nil
	case syncStartedMsg:
		for id, mode := range msg.modes {
			m.syncedModes[id] = mode
			delete(m.selectedItems, id)
		}
		m.refreshVisibleMarkers()
		if len(msg.errs) > 0 {
			m.status = fmt.Sprintf("%d item(ns) iniciado(s), %d erro(s): %s", msg.count, len(msg.errs), strings.Join(msg.errs, " | "))
		} else {
			m.status = fmt.Sprintf("%d item(ns) configurado(s). O servidor iniciou o sync em background.", msg.count)
		}
		return m, m.waitForStatus()
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
		m.status = "Erro: " + msg.text
		return m, nil
	case tea.WindowSizeMsg:
		h, v := docStyle.GetFrameSize()
		m.list.SetSize(msg.Width-h, msg.Height-v-5)
	case statusMsg:
		m.status = fmt.Sprintf("%s: %s", msg.Status, msg.FilePath)
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
	} else if m.state == stateLocalPath {
		s = docStyle.Render(fmt.Sprintf("%s\n\n%s", m.status, m.pathInput.View()))
	} else {
		s = docStyle.Render(m.list.View())
		s += "\n\n" + m.status + "\n"
	}
	s += "\n" + m.progress.View() + "\n\n"
	s += "ctrl+c sair | backspace provedores | b base-sync | f full-sync | p caminho\n"
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
