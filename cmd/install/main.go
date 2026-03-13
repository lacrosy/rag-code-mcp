package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Flags
var (
	ollamaMode    = flag.String("ollama", "local", "Ollama mode: 'local' (existing) or 'docker' (container)")
	qdrantMode    = flag.String("qdrant", "docker", "Qdrant mode: 'docker' (container) or 'remote' (existing URL)")
	gpu           = flag.Bool("gpu", false, "Enable GPU for Docker containers")
	skipBuild     = flag.Bool("skip-build", false, "Skip building binaries (use pre-built from release)")
	installDir    = flag.String("dir", "./ragcode", "Installation directory (relative to CWD or absolute)")
	idesFlag      = flag.String("ides", "auto", "IDE list: auto, claude, cursor, vs-code, windsurf, copilot, antigravity")
	upgradeFlag   = flag.Bool("upgrade", false, "Upgrade existing installation")
	uninstallFlag = flag.Bool("uninstall", false, "Uninstall and clean up")
)

// Constants
const (
	ollamaImage         = "ollama/ollama:latest"
	qdrantImage         = "qdrant/qdrant:latest"
	phpBridgeImage      = "ragcode-php-bridge:latest"
	ollamaContainer     = "ragcode-ollama"
	qdrantContainer     = "ragcode-qdrant"
	phpBridgeContainer  = "ragcode-php-bridge"
	defaultModel        = "phi3:medium"
	defaultEmbed        = "nomic-embed-text"
	repoURL             = "https://github.com/lacrosy/rag-code-mcp"
)

// Colors
var (
	blue   = "\033[0;34m"
	green  = "\033[0;32m"
	yellow = "\033[1;33m"
	red    = "\033[0;31m"
	reset  = "\033[0m"
)

func init() {
	if runtime.GOOS == "windows" {
		blue, green, yellow, red, reset = "", "", "", "", ""
	}
}

func log(msg string)     { fmt.Printf("%s==> %s%s\n", blue, msg, reset) }
func success(msg string) { fmt.Printf("%s✓ %s%s\n", green, msg, reset) }
func warn(msg string)    { fmt.Printf("%s! %s%s\n", yellow, msg, reset) }
func fail(msg string)    { fmt.Printf("%s✗ %s%s\n", red, msg, reset); os.Exit(1) }

func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// resolveInstallDir returns absolute path for the install directory.
func resolveInstallDir() string {
	dir := *installDir
	if !filepath.IsAbs(dir) {
		wd, err := os.Getwd()
		if err != nil {
			fail(fmt.Sprintf("Cannot determine working directory: %v", err))
		}
		dir = filepath.Join(wd, dir)
	}
	return filepath.Clean(dir)
}

func main() {
	flag.Parse()

	dir := resolveInstallDir()

	if *uninstallFlag {
		runUninstall(dir)
		return
	}

	printBanner()

	if *upgradeFlag {
		log("Upgrading RagCode...")
	}

	// 0. Docker check
	if *ollamaMode == "docker" || *qdrantMode == "docker" {
		checkDockerAvailable()
		freeRequiredPorts()
	}

	// 1. Build / download binaries → dir/bin/
	installBinaries(dir)

	// 2. Install PHP bridge → dir/php-bridge/
	installPHPBridge(dir)

	// 3. Create default config → dir/config.yaml
	installConfig(dir)

	// 4. Setup services (Docker containers)
	setupServices()

	// 5. Pull models
	provisionModels()

	// 6. Configure IDEs
	configureIDEs(dir, parseIDESelections(*idesFlag))

	// 7. Health check
	runHealthCheck(dir)

	printSummary(dir)
}

// ─── Step 1: Binaries ──────────────────────────────────────────────────────────

func installBinaries(dir string) {
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		fail(fmt.Sprintf("Cannot create %s: %v", binDir, err))
	}

	binaries := []struct {
		name string
		pkg  string
	}{
		{"rag-code-mcp", "./cmd/rag-code-mcp"},
		{"index-all", "./cmd/index-all"},
	}

	// 1. Check if pre-built binaries exist in CWD (extracted from release archive)
	allFound := true
	for _, bin := range binaries {
		name := bin.name
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		if _, err := os.Stat(name); err != nil {
			allFound = false
			break
		}
	}
	if allFound {
		log("Copying pre-built binaries from current directory...")
		for _, bin := range binaries {
			name := bin.name
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			if err := copyFile(name, filepath.Join(binDir, name)); err != nil {
				fail(fmt.Sprintf("Failed to copy %s: %v", name, err))
			}
			_ = os.Chmod(filepath.Join(binDir, name), 0755)
			success(fmt.Sprintf("Installed %s", name))
		}
		return
	}

	if *skipBuild {
		// Try downloading pre-built release
		if downloadRelease(binDir) {
			return
		}
		warn("Download failed, falling back to build from source.")
	}

	// 2. Build from source if go.mod is present
	if _, err := os.Stat("go.mod"); err == nil {
		log("Building from source...")
		for _, bin := range binaries {
			outName := bin.name
			if runtime.GOOS == "windows" {
				outName += ".exe"
			}
			output := filepath.Join(binDir, outName)
			cmd := exec.Command("go", "build", "-o", output, bin.pkg)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				fail(fmt.Sprintf("Failed to build %s: %v", bin.name, err))
			}
			_ = os.Chmod(output, 0755)
			success(fmt.Sprintf("Built %s", outName))
		}
		return
	}

	// 3. Last resort: download from GitHub
	if !downloadRelease(binDir) {
		fail("Cannot install binaries. Run from source tree, from extracted release, or ensure GitHub releases are available.")
	}
}

func downloadRelease(binDir string) bool {
	var archiveName string
	arch := runtime.GOARCH
	switch runtime.GOOS {
	case "linux":
		archiveName = fmt.Sprintf("rag-code-mcp_linux_%s.tar.gz", arch)
	case "darwin":
		archiveName = fmt.Sprintf("rag-code-mcp_darwin_%s.tar.gz", arch)
	case "windows":
		archiveName = fmt.Sprintf("rag-code-mcp_windows_%s.zip", arch)
	default:
		return false
	}

	url := fmt.Sprintf("%s/releases/latest/download/%s", repoURL, archiveName)
	log(fmt.Sprintf("Downloading %s...", url))

	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		warn("Release download failed.")
		return false
	}
	defer resp.Body.Close()

	tmpFile, err := os.CreateTemp("", "ragcode-*.tar.gz")
	if err != nil {
		return false
	}
	defer os.Remove(tmpFile.Name())

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		return false
	}
	tmpFile.Close()

	// Extract to binDir
	cmd := exec.Command("tar", "-xzf", tmpFile.Name(), "-C", binDir)
	if err := cmd.Run(); err != nil {
		warn(fmt.Sprintf("Failed to extract archive: %v", err))
		return false
	}

	success("Downloaded and extracted release binaries")
	return true
}

// ─── Step 2: PHP Bridge ────────────────────────────────────────────────────────

func installPHPBridge(dir string) {
	bridgeDir := filepath.Join(dir, "php-bridge")

	// Check if source php-bridge/ exists (running from source tree)
	if _, err := os.Stat("php-bridge/parse.php"); err == nil {
		log("Installing PHP bridge from source tree...")
		if err := os.RemoveAll(bridgeDir); err != nil && !os.IsNotExist(err) {
			fail(fmt.Sprintf("Cannot clean %s: %v", bridgeDir, err))
		}
		if err := copyDir("php-bridge", bridgeDir); err != nil {
			fail(fmt.Sprintf("Failed to copy php-bridge: %v", err))
		}
	} else if _, err := os.Stat(filepath.Join(bridgeDir, "parse.php")); err != nil {
		// Not in source tree and no existing bridge — clone just the php-bridge
		log("Cloning PHP bridge from repository...")
		tmpDir, err := os.MkdirTemp("", "ragcode-clone-*")
		if err != nil {
			fail(fmt.Sprintf("Cannot create temp dir: %v", err))
		}
		defer os.RemoveAll(tmpDir)

		cmd := exec.Command("git", "clone", "--depth", "1", "--filter=blob:none", "--sparse",
			repoURL+".git", tmpDir)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fail(fmt.Sprintf("Failed to clone repo: %v", err))
		}

		sparseCmd := exec.Command("git", "-C", tmpDir, "sparse-checkout", "set", "php-bridge")
		if err := sparseCmd.Run(); err != nil {
			fail(fmt.Sprintf("Failed to sparse-checkout: %v", err))
		}

		if err := copyDir(filepath.Join(tmpDir, "php-bridge"), bridgeDir); err != nil {
			fail(fmt.Sprintf("Failed to copy php-bridge: %v", err))
		}
	} else {
		log("PHP bridge already exists, skipping copy.")
	}

	// Run composer install
	if !commandExists("composer") {
		warn("Composer not found. Install it: https://getcomposer.org/download/")
		warn("Then run: cd " + bridgeDir + " && composer install --no-dev")
		return
	}
	if !commandExists("php") {
		warn("PHP CLI not found. Install PHP 8.1+.")
		warn("Then run: cd " + bridgeDir + " && composer install --no-dev")
		return
	}

	log("Running composer install...")
	cmd := exec.Command("composer", "install", "--no-dev", "--optimize-autoloader", "--quiet")
	cmd.Dir = bridgeDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		warn(fmt.Sprintf("Composer install failed: %v. Run manually: cd %s && composer install --no-dev", err, bridgeDir))
		return
	}
	success("PHP bridge installed (nikic/php-parser)")
}

// ─── Step 3: Config ────────────────────────────────────────────────────────────

func installConfig(dir string) {
	configPath := filepath.Join(dir, "config.yaml")
	if _, err := os.Stat(configPath); err == nil {
		log("Config exists, skipping: " + configPath)
		return
	}

	log("Creating default config.yaml...")
	config := `llm:
  provider: ollama
  ollama_base_url: http://localhost:11434
  ollama_model: ` + defaultModel + `
  ollama_embed: ` + defaultEmbed + `
  temperature: 0.7
  max_tokens: 1024
  timeout: 60s
  max_retries: 3

storage:
  vector_db:
    url: http://localhost:6333
    api_key: ""

logging:
  level: info
  format: text
  output: file
  path: ragcode.log

rag_code:
  enabled: true
  index_on_startup: false

workspace:
  enabled: true
  auto_index: false
  max_workspaces: 10
  detection_markers:
    - composer.json
  collection_prefix: ragcode
  index_include: []
  index_exclude:
    - vendor
    - node_modules
    - .git
    - docker
    - docs
  exclude_patterns:
    - "*.generated.php"
    - "*.cache.php"
`
	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		fail(fmt.Sprintf("Cannot write config: %v", err))
	}
	success("Created " + configPath)
}

// ─── Step 4: Services ──────────────────────────────────────────────────────────

func setupServices() {
	log("Configuring services...")
	client := &http.Client{Timeout: 2 * time.Second}

	// Qdrant
	qdrantReady := false
	if resp, err := client.Get("http://127.0.0.1:6333/readyz"); err == nil {
		resp.Body.Close()
		qdrantReady = true
		success("Qdrant already running on port 6333")
	}
	if !qdrantReady && *qdrantMode == "docker" {
		startDockerContainer(qdrantContainer, qdrantImage,
			[]string{"-p", "6333:6333", "-p", "6334:6334", "-v", "ragcode_qdrant_data:/qdrant/storage"}, nil)
	}

	// Ollama
	ollamaReady := false
	if resp, err := client.Get("http://127.0.0.1:11434/api/tags"); err == nil {
		resp.Body.Close()
		ollamaReady = true
		success("Ollama already running on port 11434")
	}
	if !ollamaReady && *ollamaMode == "docker" {
		args := []string{"-p", "11434:11434", "-v", "ragcode_ollama_data:/root/.ollama"}
		if *gpu {
			args = append(args, "--gpus", "all")
		}
		startDockerContainer(ollamaContainer, ollamaImage, args, nil)
	}

	// PHP Bridge — if no local PHP, start container
	phpBridgeReady := false
	if resp, err := client.Get("http://127.0.0.1:9100/health"); err == nil {
		resp.Body.Close()
		phpBridgeReady = true
		success("PHP bridge already running on port 9100")
	}
	if !phpBridgeReady {
		if commandExists("php") && commandExists("composer") {
			success("PHP + Composer available locally (CLI mode)")
		} else {
			// Need Docker container for PHP bridge
			log("PHP/Composer not found locally. Starting PHP bridge container...")
			dir := resolveInstallDir()
			bridgeDir := filepath.Join(dir, "php-bridge")
			if _, err := os.Stat(filepath.Join(bridgeDir, "Dockerfile")); err == nil {
				// Build image from local Dockerfile
				buildCmd := exec.Command("docker", "build", "-t", phpBridgeImage, bridgeDir)
				buildCmd.Stdout = os.Stdout
				buildCmd.Stderr = os.Stderr
				if err := buildCmd.Run(); err != nil {
					warn(fmt.Sprintf("Failed to build PHP bridge image: %v", err))
				} else {
					projectDir, _ := os.Getwd()
					startDockerContainer(phpBridgeContainer, phpBridgeImage,
						[]string{"-p", "9100:9100", "-v", projectDir + ":/workspace:ro"}, nil)
				}
			} else {
				warn("PHP bridge Dockerfile not found. Either install PHP 8.1+ or provide php-bridge/Dockerfile")
			}
		}
	}

	waitForService("Ollama", "http://localhost:11434")
	waitForService("Qdrant", "http://localhost:6333/readyz")
}

func startDockerContainer(name, image string, args, env []string) {
	// Check if already running
	out, _ := exec.Command("docker", "ps", "-q", "-f", "name="+name).Output()
	if len(bytes.TrimSpace(out)) > 0 {
		success(fmt.Sprintf("Container %s already running", name))
		return
	}

	// Remove stopped container
	_ = exec.Command("docker", "rm", name).Run()

	runArgs := []string{"run", "-d", "--name", name, "--restart", "unless-stopped"}
	runArgs = append(runArgs, args...)
	for _, e := range env {
		runArgs = append(runArgs, "-e", e)
	}
	runArgs = append(runArgs, image)

	log(fmt.Sprintf("Starting %s...", name))
	if err := exec.Command("docker", runArgs...).Run(); err != nil {
		fail(fmt.Sprintf("Failed to start %s: %v", name, err))
	}
	success(fmt.Sprintf("Started %s", name))
}

func waitForService(name, url string) {
	log(fmt.Sprintf("Waiting for %s...", name))
	for i := 0; i < 30; i++ {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			success(fmt.Sprintf("%s ready", name))
			return
		}
		time.Sleep(1 * time.Second)
	}
	fail(fmt.Sprintf("%s failed to start after 30s", name))
}

// ─── Step 5: Models ────────────────────────────────────────────────────────────

type ModelList struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

func provisionModels() {
	log("Checking AI models...")
	required := []string{defaultModel, defaultEmbed}

	resp, err := http.Get("http://localhost:11434/api/tags")
	if err != nil {
		warn("Cannot reach Ollama API. Skipping model provisioning.")
		return
	}
	defer resp.Body.Close()

	var list ModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		warn("Failed to parse Ollama model list")
		return
	}

	installed := make(map[string]bool)
	for _, m := range list.Models {
		installed[m.Name] = true
	}

	for _, req := range required {
		found := false
		for k := range installed {
			if strings.HasPrefix(k, req) {
				found = true
				break
			}
		}
		if found {
			success(fmt.Sprintf("Model %s present", req))
		} else {
			pullModel(req)
		}
	}
}

func pullModel(name string) {
	log(fmt.Sprintf("Pulling model %s (may take a while)...", name))
	reqBody, _ := json.Marshal(map[string]string{"name": name})
	resp, err := http.Post("http://localhost:11434/api/pull", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		fail(fmt.Sprintf("Failed to pull %s: %v", name, err))
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		status, _ := chunk["status"].(string)
		percent := ""
		if completed, ok := chunk["completed"].(float64); ok {
			if total, ok := chunk["total"].(float64); ok && total > 0 {
				percent = fmt.Sprintf(" %.0f%%", (completed/total)*100)
			}
		}
		fmt.Printf("\r   ↳ %s%s", status, percent)
		if status == "success" {
			break
		}
	}
	fmt.Println()
	success(fmt.Sprintf("Model %s ready", name))
}

// ─── Step 6: IDE Configuration ─────────────────────────────────────────────────

func configureIDEs(dir string, selected []string) {
	log("Configuring IDEs...")
	home, err := os.UserHomeDir()
	if err != nil {
		warn("Cannot determine home directory, skipping IDE config")
		return
	}

	binPath := filepath.Join(dir, "bin", "rag-code-mcp")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	configPath := filepath.Join(dir, "config.yaml")
	bridgePath := filepath.Join(dir, "php-bridge", "parse.php")

	paths := resolveIDEPaths(home)
	selection := normalizeIdeSelection(selected)

	for key, cfg := range paths {
		shouldConfigure := selection.explicit[key]
		if !selection.auto && !shouldConfigure {
			continue
		}
		cfgDir := filepath.Dir(cfg.path)
		if !shouldConfigure {
			if _, err := os.Stat(cfgDir); err != nil {
				continue
			}
		} else {
			_ = os.MkdirAll(cfgDir, 0755)
		}
		updateMCPConfig(key, cfg.displayName, cfg.path, binPath, configPath, bridgePath)
	}
}

type idePath struct {
	path        string
	displayName string
}

func resolveIDEPaths(home string) map[string]idePath {
	paths := map[string]idePath{
		"windsurf": {filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), "Windsurf"},
		"cursor":   {filepath.Join(home, ".cursor", "mcp.config.json"), "Cursor"},
		"copilot":  {filepath.Join(home, ".aitk", "mcp.json"), "GitHub Copilot"},
		"antigravity": {filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"), "Antigravity"},
		"mcp-cli":  {filepath.Join(home, ".config", "mcp-servers.json"), "MCP CLI"},
	}
	switch runtime.GOOS {
	case "darwin":
		paths["claude"] = idePath{filepath.Join(home, "Library", "Application Support", "Claude", "mcp-servers.json"), "Claude Desktop"}
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			paths["claude"] = idePath{filepath.Join(appData, "Claude", "mcp-servers.json"), "Claude Desktop"}
		}
	default:
		paths["claude"] = idePath{filepath.Join(home, ".config", "Claude", "mcp-servers.json"), "Claude Desktop"}
	}
	if vsPath, ok := determineVSCodePath(home); ok {
		paths["vs-code"] = vsPath
	}
	return paths
}

func determineVSCodePath(home string) (idePath, bool) {
	var userDir string
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			userDir = filepath.Join(appData, "Code", "User")
		} else {
			return idePath{}, false
		}
	case "darwin":
		userDir = filepath.Join(home, "Library", "Application Support", "Code", "User")
	default:
		userDir = filepath.Join(home, ".config", "Code", "User")
	}
	newPath := filepath.Join(userDir, "mcp.json")
	legacyPath := filepath.Join(userDir, "globalStorage", "mcp-servers.json")
	chosen := newPath
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		if _, err := os.Stat(legacyPath); err == nil {
			chosen = legacyPath
		}
	}
	return idePath{chosen, "VS Code"}, true
}

func parseIDESelections(raw string) []string {
	if raw == "" {
		return nil
	}
	var result []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

type ideSelection struct {
	auto     bool
	explicit map[string]bool
}

func normalizeIdeSelection(selected []string) ideSelection {
	if len(selected) == 0 {
		return ideSelection{auto: true, explicit: map[string]bool{}}
	}
	sel := ideSelection{explicit: map[string]bool{}}
	for _, item := range selected {
		if item == "auto" {
			sel.auto = true
		} else {
			sel.explicit[item] = true
		}
	}
	if len(sel.explicit) == 0 {
		sel.auto = true
	}
	return sel
}

func updateMCPConfig(ideKey, displayName, path, binPath, configPath, bridgePath string) {
	config := make(map[string]interface{})
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &config)
	}

	collectionKey := "mcpServers"
	if ideKey == "vs-code" || ideKey == "copilot" {
		collectionKey = "servers"
	}

	servers := make(map[string]interface{})
	if existing, ok := config[collectionKey].(map[string]interface{}); ok {
		servers = existing
	}

	// Clean up legacy keys
	for _, lk := range []string{"coderag", "do-ai", "ragcond"} {
		delete(servers, lk)
	}

	env := map[string]string{
		"RAGCODE_PHP_BRIDGE": bridgePath,
	}
	// If PHP is not available locally, assume Docker bridge on port 9100
	if !commandExists("php") {
		env["RAGCODE_PHP_BRIDGE_URL"] = "http://localhost:9100"
	}
	entry := map[string]interface{}{
		"command": binPath,
		"args":    []string{"-config", configPath},
		"env":     env,
	}

	if ideKey == "vs-code" || ideKey == "copilot" {
		entry["alwaysAllow"] = []string{
			"search_code", "search_local_index", "get_function_details",
			"find_type_definition", "get_code_context", "list_package_exports",
			"find_implementations", "search_docs", "hybrid_search", "index_workspace",
		}
	}
	if ideKey == "windsurf" {
		entry["disabled"] = false
	}

	servers["ragcode"] = entry
	config[collectionKey] = servers

	data, _ := json.MarshalIndent(config, "", "  ")
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	if err := os.WriteFile(path, data, 0644); err != nil {
		warn(fmt.Sprintf("Failed to write %s: %v", path, err))
	} else {
		success(fmt.Sprintf("Configured %s", displayName))
	}
}

// ─── Step 7: Health Check ──────────────────────────────────────────────────────

func runHealthCheck(dir string) {
	log("Running health check...")
	binPath := filepath.Join(dir, "bin", "rag-code-mcp")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	if _, err := os.Stat(binPath); err != nil {
		warn("Binary not found, skipping health check")
		return
	}

	cmd := exec.Command(binPath, "--health")
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		warn("Health check reported issues. Run manually: " + binPath + " --health")
	} else {
		success("Health check passed")
	}
}

// ─── Uninstall ─────────────────────────────────────────────────────────────────

func runUninstall(dir string) {
	log("Uninstalling RagCode from " + dir + "...")

	// Stop Docker containers
	if commandExists("docker") {
		_ = exec.Command("docker", "stop", ollamaContainer, qdrantContainer, phpBridgeContainer).Run()
		_ = exec.Command("docker", "rm", ollamaContainer, qdrantContainer, phpBridgeContainer).Run()
		success("Docker containers removed")
	}

	// Remove IDE configs
	home, _ := os.UserHomeDir()
	if home != "" {
		for key, ide := range resolveIDEPaths(home) {
			removeConfigFromIDE(key, ide.path, ide.displayName)
		}
	}

	// Remove install directory
	if err := os.RemoveAll(dir); err != nil {
		warn(fmt.Sprintf("Could not remove %s: %v", dir, err))
	} else {
		success("Removed " + dir)
	}

	success("RagCode uninstalled.")
}

func removeConfigFromIDE(ideKey, path, displayName string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return
	}

	collectionKey := "mcpServers"
	if ideKey == "vs-code" || ideKey == "copilot" {
		collectionKey = "servers"
	}
	if _, ok := config["mcp-servers"]; ok {
		collectionKey = "mcp-servers"
	}

	servers, ok := config[collectionKey].(map[string]interface{})
	if !ok {
		return
	}
	if _, ok := servers["ragcode"]; ok {
		delete(servers, "ragcode")
		updated, _ := json.MarshalIndent(config, "", "  ")
		if err := os.WriteFile(path, updated, 0644); err == nil {
			log(fmt.Sprintf("Removed config from %s", displayName))
		}
	}
}

// ─── Docker checks ─────────────────────────────────────────────────────────────

func checkDockerAvailable() {
	log("Checking Docker...")
	if !commandExists("docker") {
		fail("Docker not found. Install Docker: https://docs.docker.com/get-docker/")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		fail("Docker daemon not running. Start Docker and try again.")
	}
	success("Docker available")
}

func isPortInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func freeRequiredPorts() {
	ports := map[int]string{}
	if *ollamaMode == "docker" {
		ports[11434] = "Ollama"
	}
	if *qdrantMode == "docker" {
		ports[6333] = "Qdrant"
	}
	if !commandExists("php") {
		ports[9100] = "PHP Bridge"
	}

	client := &http.Client{Timeout: 2 * time.Second}
	for port, name := range ports {
		if !isPortInUse(port) {
			continue
		}
		// Check if it's our container
		containerName := qdrantContainer
		if port == 11434 {
			containerName = ollamaContainer
		}
		out, _ := exec.Command("docker", "ps", "--filter", "name="+containerName, "--filter", "status=running", "--format", "{{.Names}}").Output()
		if strings.Contains(string(out), containerName) {
			success(fmt.Sprintf("%s already running in Docker", name))
			continue
		}
		// Check if the expected service is responding
		var checkURL string
		if port == 11434 {
			checkURL = "http://127.0.0.1:11434/api/tags"
		} else {
			checkURL = "http://127.0.0.1:6333/readyz"
		}
		if resp, err := client.Get(checkURL); err == nil {
			resp.Body.Close()
			success(fmt.Sprintf("%s already running on port %d", name, port))
			continue
		}
		fail(fmt.Sprintf("Port %d (%s) is in use by an unknown process. Free it and try again.", port, name))
	}
}

// ─── Helpers ───────────────────────────────────────────────────────────────────

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target)
	})
}

// ─── Banner & Summary ──────────────────────────────────────────────────────────

func printBanner() {
	fmt.Println(`
    ____              ______          __
   / __ \____ _____ _/ ____/___  ____/ /__
  / /_/ / __ '/ __ '/ /   / __ \/ __  / _ \
 / _, _/ /_/ / /_/ / /___/ /_/ / /_/ /  __/
/_/ |_|\__,_/\__, /\____/\____/\__,_/\___/
            /____/
   Local Installer`)
}

func printSummary(dir string) {
	binPath := filepath.Join(dir, "bin", "rag-code-mcp")
	configPath := filepath.Join(dir, "config.yaml")
	bridgePath := filepath.Join(dir, "php-bridge", "parse.php")

	fmt.Printf("\n%sInstallation Complete!%s\n", green, reset)
	fmt.Println("────────────────────────────────────────────")
	fmt.Printf("  Directory:  %s\n", dir)
	fmt.Printf("  Binary:     %s\n", binPath)
	fmt.Printf("  Config:     %s\n", configPath)
	if commandExists("php") {
		fmt.Printf("  PHP Bridge: %s (CLI mode)\n", bridgePath)
	} else {
		fmt.Printf("  PHP Bridge: http://localhost:9100 (Docker mode)\n")
	}
	fmt.Println()
	fmt.Println("MCP server entry for .mcp.json:")
	if commandExists("php") {
		fmt.Printf(`  {
    "ragcode": {
      "command": "%s",
      "args": ["-config", "%s"],
      "env": { "RAGCODE_PHP_BRIDGE": "%s" }
    }
  }
`, binPath, configPath, bridgePath)
	} else {
		fmt.Printf(`  {
    "ragcode": {
      "command": "%s",
      "args": ["-config", "%s"],
      "env": { "RAGCODE_PHP_BRIDGE_URL": "http://localhost:9100" }
    }
  }
`, binPath, configPath)
	}
	fmt.Println()
	fmt.Println("Next: open your IDE and ask AI to index_workspace.")
}
