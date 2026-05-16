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
	"github.com/sahilm/fuzzy"
	"github.com/thiagohmm/insync-clone/api/proto/insync"
	igrpc "github.com/thiagohmm/insync-clone/internal/infrastructure/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fuzzyFilter wraps sahilm/fuzzy into the bubbles/list FilterFunc signature.
func fuzzyFilter(term string, targets []string) []list.Rank {
	results := fuzzy.Find(term, targets)
	ranks := make([]list.Rank, 0, len(results))
	for _, r := range results {
		rank := list.Rank{
			Index:          r.Index,
			MatchedIndexes: r.MatchedIndexes,
		}
		ranks = append(ranks, rank)
	}
	return ranks
}

var docStyle = lipgloss.NewStyle().Margin(1, 2)

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
	// Exibição:
	// - [] para itens que sofreram unsync (não têm mode mais)
	// - [B] para base-sync
	// - [F] para full-sync
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

type state int

const (
	stateAuthCode state = iota
	stateBrowsing
	stateLocalPath
	stateConfirmExit
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
	authCode      string
	browsePath    []string // do root ao diretório atual, ex.: {"root", "abc..."}
	syncedModes   map[string]insync.SyncMode
	selectedItems map[string]browseItem
	lastLocalRoot string
	confirmText   string // "s" ou "n"
}

func initialModel(client insync.InsyncServiceClient) model {
	ti := textinput.New()
	ti.Placeholder = "aguardando autenticação no navegador..."
	ti.Prompt = ""
	ti.CharLimit = 512
	ti.Width = 80
	m := model{
		list:          list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0),
		progress:      progress.New(progress.WithDefaultGradient()),
		pathInput:     ti,
		status:        "Conectado ao Daemon. Iniciando autenticação...",
		client:        client,
		state:         stateAuthCode,
		syncedModes:   make(map[string]insync.SyncMode),
		selectedItems: make(map[string]browseItem),
	}
	m.list.Title = "Insync Clone - Google Drive"
	m.list.SetFilteringEnabled(true)
	m.list.SetShowFilter(true)
	m.list.Filter = fuzzyFilter
	m.list.DisableQuitKeybindings()

	// Substituir KeyMap para evitar conflito com 'b' e 'f'
	m.list.KeyMap.PrevPage.SetKeys("left", "h", "pgup")
	m.list.KeyMap.NextPage.SetKeys("right", "l", "pgdown")
	m.list.KeyMap.PrevPage.SetHelp("←/h/pgup", "prev page")
	m.list.KeyMap.NextPage.SetHelp("→/l/pgdn", "next page")
	m.ctx, m.cancel = context.WithCancel(context.Background())

	// Obter URL de autenticação e abrir navegador automaticamente
	resp, err := client.GetAuthURL(m.ctx, &insync.GetAuthURLRequest{})
	if err != nil {
		m.status = fmt.Sprintf("Erro ao obter URL de autenticação: %v", err)
	} else {
		m.status = fmt.Sprintf("🌐 Abrindo navegador para autenticação Google Drive...\n\n📋 Se não abrir automaticamente, copie e cole no navegador:\n%s\n\naguardando autorização...", resp.Url)
		// Tentar abrir o navegador automaticamente
		go func() {
			cmd := exec.Command("open", resp.Url) // macOS
			if cmd.Run() != nil {
				cmd = exec.Command("xdg-open", resp.Url) // Linux
				if cmd.Run() != nil {
					exec.Command("cmd", "/c", "start", resp.Url).Run() // Windows
				}
			}
		}()
	}

	return m
}

func (m model) afterAuthSuccess() (model, tea.Cmd) {
	m.state = stateBrowsing
	m.browsePath = []string{"root"}
	m.list.Title = "Google Drive — b: base-sync | f: full-sync"
	m.status = "Autenticado! Carregando lista da nuvem..."
	return m, tea.Batch(m.fetchFiles("root"), m.fetchSynced())
}

func (m model) Init() tea.Cmd {
	// Se estiver em stateAuthCode, iniciar polling para verificar se a autenticação foi concluída
	if m.state == stateAuthCode {
		return m.checkAuthStatus()
	}
	return nil
}

func (m model) checkAuthStatus() tea.Cmd {
	return func() tea.Msg {
		time.Sleep(2 * time.Second)

		// Tentar verificar se já existe uma conta autenticada
		res, err := m.client.AddAccount(m.ctx, &insync.AddAccountRequest{
			AuthCode: "", // Código vazio significa: "verificar se já existe conta"
		})

		if err == nil && res.Success && res.AccountId != "" {
			return res // Retorna a resposta com sucesso
		}

		// Ainda não autenticou, tentar novamente
		return m.checkAuthStatus()
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

	// Se o item já está em syncedModes (syncado anteriormente), mover para selectedItems
	// para permitir mudança de mode
	if _, ok := m.syncedModes[sel.remoteID]; ok {
		// Item já syncado, remover de syncedModes e adicionar a selectedItems
		delete(m.syncedModes, sel.remoteID)
	}

	sel.mode = mode
	sel.hasMode = true
	// Criar cópia do mapa para evitar aliasing com o modelo original
	newSelectedItems := make(map[string]browseItem)
	for k, v := range m.selectedItems {
		newSelectedItems[k] = v
	}
	newSelectedItems[sel.remoteID] = sel
	m.selectedItems = newSelectedItems
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

func (m model) unsyncSelected() (model, tea.Cmd) {
	sel, ok := m.list.SelectedItem().(browseItem)
	if !ok || sel.isBack {
		return m, nil
	}

	// Verificar se o item está syncado (tanto em syncedModes quanto em selectedItems)
	_, inSyncedModes := m.syncedModes[sel.remoteID]
	_, inSelectedItems := m.selectedItems[sel.remoteID]

	if !inSyncedModes && !inSelectedItems {
		m.status = "Este item não está sincronizado."
		return m, nil
	}

	m.status = fmt.Sprintf("Removendo sync de %s... (apenas local)", sel.title)
	return m, m.performUnsync(sel.remoteID, sel.title)
}

func (m *model) performUnsync(remoteID string, title string) tea.Cmd {
	return func() tea.Msg {
		// Chamada RPC para desfazer o sync
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()

		res, err := m.client.Unsync(ctx, &insync.UnsyncRequest{
			AccountId:      m.accountID,
			RemoteFolderId: remoteID,
		})

		if err != nil {
			return listErrMsg{text: fmt.Sprintf("erro ao remover sync de %s: %v", title, err)}
		}

		if !res.Success {
			return listErrMsg{text: fmt.Sprintf("erro ao remover sync de %s: %s", title, res.GetErrorMessage())}
		}

		// Remover do mapa de modes locais
		delete(m.syncedModes, remoteID)
		delete(m.selectedItems, remoteID)

		return syncedListMsg{modes: m.syncedModes, localRoot: m.lastLocalRoot}
	}
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

		// --- stateLocalPath ---
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

		// --- stateConfirmExit (caixa de confirmação) ---
		if m.state == stateConfirmExit {
			switch key {
			case "esc":
				m.state = stateBrowsing
				m.confirmText = ""
				m.status = "Sessão mantida."
				return m, nil
			case "enter":
				if strings.ToLower(m.confirmText) == "s" {
					m.state = stateAuthCode
					m.authCode = ""
					m.browsePath = nil
					m.list.ResetSelected()
					m.list.ResetFilter()
					m.confirmText = ""
					return m, nil
				}
				// "n" ou qualquer outra coisa → volta para browsing
				m.state = stateBrowsing
				m.confirmText = ""
				m.status = "Sessão mantida."
				return m, nil
			case "backspace":
				if len(m.confirmText) > 0 {
					m.confirmText = m.confirmText[:len(m.confirmText)-1]
				}
				return m, nil
			default:
				if len(key) == 1 {
					m.confirmText += key
					m.status = fmt.Sprintf("Sair da sessão? (%s) s/n", m.confirmText)
					return m, nil
				}
			}
			return m, nil
		}

		// --- stateBrowsing ---
		if m.state == stateBrowsing {
			// Primeiro, tenta processar a tecla na lista (isso inclui o filter)
			// O bubbles/list vai tratar teclas de navegação/filter e gerar um cmd
			var cmd tea.Cmd
			m.list, cmd = m.list.Update(msg)

			// Se a lista gerou um cmd, ela processou a tecla (navegação ou filter)
			if cmd != nil {
				return m, cmd
			}

			// Se a lista não processou, tenta as teclas de ação
			switch key {
			case "u":
				model, cmd := m.unsyncSelected()
				return model, cmd
			case "b":
				model, cmd := m.toggleSyncForSelection(insync.SyncMode_BASE_SYNC)
				return model, cmd
			case "f":
				model, cmd := m.toggleSyncForSelection(insync.SyncMode_FULL_SYNC)
				return model, cmd
			case "p":
				m.state = stateLocalPath
				if m.lastLocalRoot != "" {
					m.pathInput.SetValue(m.lastLocalRoot)
				} else {
					m.pathInput.SetValue(defaultSyncRoot())
				}
				var cmds []tea.Cmd
				cmds = append(cmds, m.pathInput.Focus())
				m.status = "Informe a pasta onde os itens selecionados serão sincronizados."
				var cmd tea.Cmd
				m.pathInput, cmd = m.pathInput.Update(msg)
				cmds = append(cmds, cmd)
				return m, tea.Batch(cmds...)
			case "enter":
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
				return m, nil
			}

			// Esc quando NÃO filtrando → abre confirmação de saída
			if key == "esc" && !m.list.IsFiltered() {
				m.state = stateConfirmExit
				m.confirmText = ""
				m.status = "Sair da sessão? (s/n)"
				return m, nil
			}

			// Esc quando filtrando → limpa o filter e fecha o field
			if key == "esc" && m.list.IsFiltered() {
				m.list.ResetFilter()
				return m, nil
			}
		}

		// --- stateAuthCode ---
		if m.state == stateAuthCode {
			if key == "enter" {
				res, err := m.client.AddAccount(m.ctx, &insync.AddAccountRequest{
					AuthCode: m.authCode,
				})
				if err == nil && res.Success {
					m.accountID = res.AccountId
					return m.afterAuthSuccess()
				}
				m.status = "Erro na autenticação. Verifique o código ou tente de novo no navegador."
			} else if len(key) == 1 {
				m.authCode += key
			}
			return m, nil
		}

	case fileListMsg:
		var items []list.Item
		if len(m.browsePath) > 1 {
			items = append(items, browseItem{title: "..", desc: "Voltar", isBack: true})
		}
		for _, f := range msg.files {
			desc := "Arquivo"
			if f.IsDirectory {
				desc = "Pasta — Enter abrir | u unsync | b base-sync | f full-sync"
			} else {
				desc = "Arquivo — u unsync | b base-sync | f full-sync"
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
		m.status = fmt.Sprintf("Pasta: %s — /↑↓/jk mover | Enter abrir pasta | u unsync | b base-sync | f full-sync | p caminho", cur)
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
		return m, nil
	case *insync.AddAccountResponse:
		// Resposta do polling de autenticação
		if msg.Success && msg.AccountId != "" {
			m.accountID = msg.AccountId
			return m.afterAuthSuccess()
		}
		// Ainda aguardando, continuar polling
		return m, m.checkAuthStatus()
	case listErrMsg:
		m.status = "Erro: " + msg.text
		return m, nil
	case tea.WindowSizeMsg:
		h, v := docStyle.GetFrameSize()
		m.list.SetSize(msg.Width-h, msg.Height-v-5)
	case statusMsg:
		// Only update progress when there's actual progress (not 0% initial state or 100% synced)
		if msg.Status == "Downloading" || msg.Status == "Uploading" {
			m.status = fmt.Sprintf("%s: %s (%d%%)", msg.Status, msg.FilePath, msg.ProgressPercentage)
			cmd := m.progress.SetPercent(float64(msg.ProgressPercentage) / 100.0)
			return m, cmd
		}
		m.status = fmt.Sprintf("%s: %s", msg.Status, msg.FilePath)
		return m, nil
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
		s = docStyle.Render(fmt.Sprintf("Google Drive\n\n%s\n\nCódigo: %s", m.status, m.authCode))
	} else if m.state == stateLocalPath {
		s = docStyle.Render(fmt.Sprintf("%s\n\n%s", m.status, m.pathInput.View()))
	} else if m.state == stateConfirmExit {
		s = docStyle.Render(m.list.View())
		s += "\n\n" + fmt.Sprintf("⚠ %s [%s]", m.status, m.confirmText) + "▌\n"
	} else {
		s = docStyle.Render(m.list.View())
		s += "\n\n" + m.status + "\n"
	}

	// Only show progress bar when there's actual progress happening
	if m.status != "" && (strings.Contains(m.status, "Downloading") || strings.Contains(m.status, "Uploading")) {
		s += "\n" + m.progress.View() + "\n"
	}

	s += "\n/ buscar | esc sair (s/n) | u unsync | b base-sync | f full-sync | p caminho | ctrl+c\n"
	return s
}

const serverAddr = "127.0.0.1:50051"

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

	authToken, err := igrpc.EnsureAuthToken()
	if err != nil {
		log.Fatalf("failed to initialize auth token: %v", err)
	}
	dialOptions := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	dialOptions = append(dialOptions, igrpc.AuthDialOptions(authToken)...)
	conn, err := grpc.Dial(serverAddr, dialOptions...)
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
