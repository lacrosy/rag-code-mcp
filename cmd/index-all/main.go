package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/doITmagic/rag-code-mcp/internal/config"
	"github.com/doITmagic/rag-code-mcp/internal/llm"
	"github.com/doITmagic/rag-code-mcp/internal/memory"
	"github.com/doITmagic/rag-code-mcp/internal/storage"
	"github.com/doITmagic/rag-code-mcp/internal/workspace"
)

func main() {
	var (
		configPath = flag.String("config", "", "Path to config.yaml (default: auto-discover next to binary or in CWD)")
		model      = flag.String("model", "", "Embedding model id (overrides config; empty = use config)")
		codeColl   = flag.String("code-collection", "", "Qdrant collection name for code (default: auto from workspace)")
		docsColl   = flag.String("docs-collection", "", "Qdrant collection name for docs (default: docs.collection)")
		dim        = flag.Int("dim", 0, "Vector dimension override (default: from config embedding_dim or 1024)")
		timeoutSec = flag.Int("timeout", 86400, "Indexing timeout in seconds (default: 24h for large codebases)")
		sourceDocs = flag.String("docs-source", "docs", "Source tag for docs metadata")
		recreate   = flag.Bool("recreate-collections", false, "If set, delete and recreate code/docs collections before indexing (DANGEROUS)")
		checkOnly  = flag.Bool("check", false, "Check if re-indexing is needed without indexing. Exit 0 = fresh, exit 1 = stale.")
		// Deprecated: kept for backward compatibility; prefer config.yaml workspace.index_include
		pathsCSV = flag.String("paths", "", "Comma-separated directories to index (overrides config index_include)")
	)
	flag.Parse()

	// Auto-discover config.yaml
	cfgPath := config.FindConfigFile(*configPath)
	if cfgPath == "" {
		cfgPath = "config.yaml" // will fail gracefully in config.Load
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// Resolve workspace root from config
	wsRoot := cfg.ResolveWorkspaceRoot()
	if wsRoot == "" {
		log.Fatalf("workspace.workspace_root is not set in %s — cannot determine project root", cfgPath)
	}
	if _, err := os.Stat(wsRoot); err != nil {
		log.Fatalf("workspace root does not exist: %s (resolved from config)", wsRoot)
	}
	log.Printf("📁 Workspace root: %s", wsRoot)

	// Determine embedding dimension
	embDim := cfg.Workspace.EmbeddingDim
	if embDim <= 0 {
		embDim = 1024 // default for mxbai-embed-large
	}
	if *dim > 0 {
		embDim = *dim // CLI override
	}

	// Determine collection name
	collectionPrefix := cfg.Workspace.CollectionPrefix
	if collectionPrefix == "" {
		collectionPrefix = "ragcode"
	}
	workspaceID := workspace.GenerateID(wsRoot)
	codeCollection := collectionPrefix + "-" + workspaceID + "-php"
	if *codeColl != "" {
		codeCollection = *codeColl
	}

	docsCollection := cfg.Docs.Collection
	if *docsColl != "" {
		docsCollection = *docsColl
	}

	// Determine directories to index
	indexDirs := cfg.Workspace.IndexInclude
	if len(indexDirs) == 0 {
		indexDirs = []string{"src"} // sensible default
	}

	// CLI -paths override (backward compatibility)
	if *pathsCSV != "" {
		indexDirs = nil
		for _, p := range strings.Split(*pathsCSV, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				// Normalize to relative path for scanWorkspace IndexInclude
				if filepath.IsAbs(p) {
					if rel, err := filepath.Rel(wsRoot, p); err == nil {
						p = rel
					}
				}
				indexDirs = append(indexDirs, p)
			}
		}
		// Override config IndexInclude so scanWorkspace scans only these dirs
		cfg.Workspace.IndexInclude = indexDirs
	}

	// Resolve index dirs to absolute paths (relative to workspace root)
	var paths []string
	for _, dir := range indexDirs {
		absDir := dir
		if !filepath.IsAbs(dir) {
			absDir = filepath.Join(wsRoot, dir)
		}
		if fi, err := os.Stat(absDir); err == nil && fi.IsDir() {
			paths = append(paths, absDir)
		} else {
			log.Printf("⚠️  Skipping missing directory: %s", absDir)
		}
	}

	if len(paths) == 0 {
		log.Fatalf("no valid directories to index (index_include: %v)", indexDirs)
	}

	// --check mode
	if *checkOnly {
		runCheckOnly(cfg, wsRoot)
		return // unreachable
	}

	// Setup LLM
	llmCfg := cfg.LLM
	if llmCfg.OllamaBaseURL == "" && llmCfg.BaseURL != "" {
		llmCfg.OllamaBaseURL = llmCfg.BaseURL
	}
	if llmCfg.OllamaModel == "" && llmCfg.Model != "" {
		llmCfg.OllamaModel = llmCfg.Model
	}
	if *model != "" {
		llmCfg.OllamaEmbed = *model
	} else if cfg.RagCode.Model != "" {
		llmCfg.OllamaEmbed = cfg.RagCode.Model
	}
	if llmCfg.OllamaEmbed == "" {
		llmCfg.OllamaEmbed = cfg.LLM.OllamaEmbed
	}
	llmCfg.Provider = "ollama"

	// Set PHP bridge URL from config
	if bridgeURL := cfg.Workspace.PHPBridgeURL; bridgeURL != "" {
		os.Setenv("RAGCODE_PHP_BRIDGE_URL", bridgeURL)
	}

	provider, err := llm.NewOllamaLLMProvider(llmCfg)
	if err != nil {
		log.Fatalf("ollama provider: %v", err)
	}

	qcfgCode := storage.QdrantConfig{
		URL:        cfg.Storage.VectorDB.URL,
		APIKey:     cfg.Storage.VectorDB.APIKey,
		Collection: codeCollection,
	}
	if err := waitForQdrantGRPC(cfg.Storage.VectorDB.URL, 30*time.Second); err != nil {
		log.Fatalf("qdrant grpc port did not become available in time: %v", err)
	}

	qclientCode, err := storage.NewQdrantClient(qcfgCode)
	if err != nil {
		log.Fatalf("qdrant code client: %v", err)
	}
	defer qclientCode.Close()

	if *recreate {
		log.Printf("⚠️ Recreating code collection '%s'", codeCollection)
		if err := qclientCode.DeleteCollection(ctx, codeCollection); err != nil {
			log.Fatalf("delete code collection: %v", err)
		}
	}

	if err := qclientCode.CreateCollection(ctx, codeCollection, embDim); err != nil {
		log.Fatalf("create code collection: %v", err)
	}

	// Create workspace manager
	mgr := workspace.NewManager(qclientCode, provider, cfg)

	// Use workspace root as info.Root — scanWorkspace handles IndexInclude internally
	info := &workspace.Info{
		ID:               workspaceID,
		Root:             wsRoot,
		ProjectType:      "mixed",
		CollectionPrefix: collectionPrefix,
	}

	// Index only configured languages (or all if index_languages is empty)
	for _, lang := range mgr.GetIndexLanguages() {
		fmt.Printf("🔎 Indexing %s files (incremental)...\n", capitalizeFirst(lang))
		if err := mgr.IndexLanguage(ctx, info, lang, codeCollection, false); err != nil {
			if strings.Contains(err.Error(), "no") && strings.Contains(err.Error(), "source files detected") {
				continue
			}
			log.Printf("⚠️ %s indexing warning: %v", capitalizeFirst(lang), err)
		}
	}

	fmt.Println("✅ Code indexing completed.")

	// Docs indexing
	var ltmDocs memory.LongTermMemory
	if docsCollection == "" {
		fmt.Println("ℹ️ docs.collection is empty, skipping docs indexing")
	} else {
		qcfgDocs := storage.QdrantConfig{
			URL:        cfg.Storage.VectorDB.URL,
			APIKey:     cfg.Storage.VectorDB.APIKey,
			Collection: docsCollection,
		}

		qclientDocs, err := storage.NewQdrantClient(qcfgDocs)
		if err != nil {
			log.Fatalf("qdrant docs client: %v", err)
		}
		defer qclientDocs.Close()

		if *recreate {
			log.Printf("⚠️ Recreating docs collection '%s'", docsCollection)
			if err := qclientDocs.DeleteCollection(ctx, docsCollection); err != nil {
				log.Fatalf("delete docs collection: %v", err)
			}
		}

		if err := qclientDocs.CreateCollection(ctx, docsCollection, embDim); err != nil {
			log.Fatalf("create docs collection: %v", err)
		}

		ltmDocs = storage.NewQdrantLongTermMemory(qclientDocs)
		var _ memory.LongTermMemory = ltmDocs

		readmePath := cfg.Docs.ReadmePath
		if readmePath == "" {
			readmePath = "./README.md"
		}
		if !filepath.IsAbs(readmePath) {
			readmePath = filepath.Join(wsRoot, readmePath)
		}

		docsPaths := cfg.Docs.DocsPaths
		if len(docsPaths) == 0 {
			docsPaths = []string{"./docs"}
		}

		var docFiles []string
		if fi, err := os.Stat(readmePath); err == nil && !fi.IsDir() {
			docFiles = append(docFiles, readmePath)
		}

		for _, root := range docsPaths {
			if !filepath.IsAbs(root) {
				root = filepath.Join(wsRoot, root)
			}
			_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				if strings.HasSuffix(strings.ToLower(info.Name()), ".md") {
					docFiles = append(docFiles, path)
				}
				return nil
			})
		}

		if len(docFiles) == 0 {
			fmt.Println("ℹ️ no markdown files found for docs indexing")
		} else {
			fmt.Printf("📚 Indexing %d docs file(s) into docs collection '%s' (model=%s, dim=%d) ...\n", len(docFiles), docsCollection, llmCfg.OllamaEmbed, embDim)

			indexedDocs := 0
			for _, path := range docFiles {
				if err := indexMarkdownFile(ctx, provider, ltmDocs, path, *sourceDocs); err != nil {
					log.Fatalf("docs indexing failed for %s after %d file(s): %v", path, indexedDocs, err)
				}
				indexedDocs++
			}

			fmt.Printf("✅ Indexed %d docs file(s)\n", indexedDocs)
		}
	}
}

// waitForQdrantGRPC pings Qdrant gRPC port on the host inferred from the given REST URL.
func waitForQdrantGRPC(baseURL string, timeout time.Duration) error {
	if baseURL == "" {
		baseURL = "http://localhost:6333"
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("invalid qdrant url: %w", err)
	}
	host := u.Hostname()
	port := u.Port()
	var grpcHost string
	if port == "" || port == "6333" {
		grpcHost = net.JoinHostPort(host, "6334")
	} else {
		grpcHost = net.JoinHostPort(host, port)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", grpcHost, 2*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timed out waiting for qdrant grpc at %s", grpcHost)
}

func indexMarkdownFile(ctx context.Context, provider llm.Provider, ltm memory.LongTermMemory, path string, source string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		chunks   []string
		current  strings.Builder
		maxChars = 1000
	)

	flushChunk := func() {
		text := strings.TrimSpace(current.String())
		if text != "" {
			chunks = append(chunks, text)
		}
		current.Reset()
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" && current.Len() > 0 {
			flushChunk()
			continue
		}

		if current.Len()+len(line)+1 > maxChars {
			flushChunk()
		}
		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(line)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", path, err)
	}
	flushChunk()

	for i, text := range chunks {
		emb, err := provider.Embed(ctx, text)
		if err != nil {
			return fmt.Errorf("embed failed for %s chunk %d: %w", path, i, err)
		}

		h := fnv.New64a()
		h.Write([]byte(fmt.Sprintf("%s#%d", path, i)))
		id := fmt.Sprintf("%d", h.Sum64())

		doc := memory.Document{
			ID:        id,
			Content:   text,
			Embedding: emb,
			Metadata: map[string]interface{}{
				"file":     path,
				"chunk_id": i,
				"source":   source,
			},
		}

		if err := ltm.Store(ctx, doc); err != nil {
			return fmt.Errorf("store failed for %s: %w", id, err)
		}
	}

	return nil
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// runCheckOnly compares state.json vs filesystem and exits with 0 (fresh) or 1 (stale).
func runCheckOnly(cfg *config.Config, wsRoot string) {
	wm := workspace.NewManager(nil, nil, cfg)

	info := &workspace.Info{
		Root:             wsRoot,
		ID:               workspace.GenerateID(wsRoot),
		ProjectType:      "configured",
		CollectionPrefix: cfg.Workspace.CollectionPrefix,
	}

	report, err := wm.CheckIndexFreshness(info)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Check failed: %v\n", err)
		os.Exit(2)
	}

	if report.Fresh {
		fmt.Printf("✅ Index is fresh (%d files, last indexed: %s)\n", report.IndexedFiles, report.LastIndexed)
		os.Exit(0)
	}

	// Stale
	total := len(report.Added) + len(report.Modified) + len(report.Deleted)
	fmt.Printf("⚠️  Index is stale (%d changes detected):\n", total)
	if len(report.Added) > 0 {
		fmt.Printf("   + %d new files\n", len(report.Added))
		for _, f := range report.Added {
			if len(report.Added) <= 10 {
				fmt.Printf("     %s\n", f)
			}
		}
		if len(report.Added) > 10 {
			fmt.Printf("     ... and %d more\n", len(report.Added)-10)
		}
	}
	if len(report.Modified) > 0 {
		fmt.Printf("   ~ %d modified files\n", len(report.Modified))
	}
	if len(report.Deleted) > 0 {
		fmt.Printf("   - %d deleted files\n", len(report.Deleted))
	}
	os.Exit(1)
}
