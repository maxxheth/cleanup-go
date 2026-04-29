package cmd

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
	"google.golang.org/genai"
)

// Roles is a custom type to handle JSON that may be a string or an array of strings.
type Roles []string

// UnmarshalJSON implements the json.Unmarshaler interface.
func (r *Roles) UnmarshalJSON(data []byte) error {
	// First, try to unmarshal as a single string.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*r = Roles{s}
		return nil
	}

	// If that fails, try to unmarshal as a slice of strings.
	var sl []string
	if err := json.Unmarshal(data, &sl); err == nil {
		*r = Roles(sl)
		return nil
	}

	return fmt.Errorf("cannot unmarshal %s into Roles", string(data))
}

// Data Structures
type Post struct {
	ID               int    `json:"ID"`
	Title            string `json:"post_title"`
	AuthorID         string `json:"post_author"`
	Date             string `json:"post_date"`
	Type             string `json:"post_type"`
	GUID             string `json:"guid"`
	ContentExcerpt   string
	Meta             map[string]string
	Author           Author
	AIClassification string
	AIJustification  string
}

type PostMetaEntry struct {
	Key   string `json:"meta_key"`
	Value string `json:"meta_value"`
}

type Author struct {
	ID          string `json:"ID"`
	DisplayName string `json:"display_name"`
	Email       string `json:"user_email"`
	Login       string `json:"user_login"`
	Roles       Roles  `json:"roles"`
}

type AIResult struct {
	Classification string `json:"classification"`
	Justification  string `json:"justification"`
}

// Global variables for flags
var (
	dockerContainer string
	outputCSVPath   string
	analyzeContent  bool
	aiProvider      string
	maxWorkers      = 10
	promptFilePath  string
	promptText      string
	postTypesArg    string
	metaKeysArg     string
	scanAllMeta     bool
	postTypes       []string
	metaKeys        []string
	metaKeysSet     map[string]struct{}
)

const promptFileEnvVar = "WP_CLEANUP_PROMPT_FILE"

var rootCmd = &cobra.Command{
	Use:   "wp-content-cleanup",
	Short: "A tool to extract and analyze WordPress content from a Docker container.",
	Long: `Extracts post and page data from a WordPress site running in a Docker
container, saves it to a CSV, and optionally analyzes the content for
spam using the Gemini AI API.`,
	Run: func(cmd *cobra.Command, args []string) {
		runApp()
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&dockerContainer, "container-name", "wordpress", "The name of the Docker container running WordPress.")
	rootCmd.PersistentFlags().StringVar(&outputCSVPath, "output-csv-path", "wp_content.csv", "The path for the output CSV file.")
	rootCmd.PersistentFlags().BoolVar(&analyzeContent, "analyze-post-content-via-ai", false, "Enable AI analysis of post content.")
	rootCmd.PersistentFlags().StringVar(&aiProvider, "ai-provider", "ollama", "The AI provider to use for content analysis. Options: 'ollama' or 'google'.")
	rootCmd.PersistentFlags().StringVar(&promptFilePath, "prompt-file", "", "Optional path to a text file containing the AI analysis prompt. Overrides the environment variable if provided.")
	rootCmd.PersistentFlags().StringVar(&postTypesArg, "post-types", "post,page", "Comma-separated post types to scan (e.g. post,page,product).")
	rootCmd.PersistentFlags().StringVar(&metaKeysArg, "meta-keys", "", "Comma-separated custom post meta keys to scan.")
	rootCmd.PersistentFlags().BoolVar(&scanAllMeta, "scan-all-meta", false, "Scan all custom meta keys for selected post types.")
}

func runApp() {
	log.Println("Welcome to the WordPress Content Cleanup Tool!")
	ctx := context.Background()

	var err error
	postTypes, err = parseCSVArg(postTypesArg)
	if err != nil {
		log.Fatalf("Invalid --post-types value: %v", err)
	}
	metaKeys, err = parseCSVArg(metaKeysArg)
	if err != nil {
		log.Fatalf("Invalid --meta-keys value: %v", err)
	}
	metaKeysSet = make(map[string]struct{}, len(metaKeys))
	for _, k := range metaKeys {
		metaKeysSet[k] = struct{}{}
	}
	if scanAllMeta && len(metaKeys) > 0 {
		log.Println("Warning: --scan-all-meta is enabled; --meta-keys will be ignored.")
	}

	promptText = defaultPrompt()
	if analyzeContent {
		if promptFilePath == "" {
			promptFilePath = os.Getenv(promptFileEnvVar)
		}
		if promptFilePath != "" {
			loadedPrompt, err := loadPromptFromFile(promptFilePath)
			if err != nil {
				log.Fatalf("Failed to load prompt file '%s': %v", promptFilePath, err)
			}
			promptText = loadedPrompt
			log.Printf("Loaded AI prompt from %s", promptFilePath)
		}
	}

	// Check if container is running
	cmd := exec.CommandContext(ctx, "docker", "inspect", dockerContainer)
	if err := cmd.Run(); err != nil {
		log.Fatalf("Docker container '%s' not found or not running. Error: %v", dockerContainer, err)
	}
	log.Printf("Successfully connected to Docker and found container '%s'", dockerContainer)

	// Initialize AI provider if needed
	if analyzeContent {
		if err := godotenv.Load(); err != nil {
			log.Println("Warning: .env file not found, relying on environment variables.")
		}
		var apiKey string
		var apiKeyName string
		if aiProvider == "ollama" {
			apiKey = os.Getenv("OLLAMA_API_KEY")
			apiKeyName = "OLLAMA_API_KEY"
		} else if aiProvider == "google" {
			apiKey = os.Getenv("GEMINI_API_KEY")
			apiKeyName = "GEMINI_API_KEY"
		} else {
			log.Fatalf("Unsupported AI provider: %s", aiProvider)
		}
		if apiKey == "" {
			log.Fatalf("%s environment variable is not set.", apiKeyName)
		}
		log.Printf("%s is set. Using %s for content analysis.\n", apiKeyName, aiProvider)
	}

	// Initialize CSV file
	csvFile, csvWriter := initializeCSV()
	defer csvFile.Close()
	defer csvWriter.Flush()

	// Get all posts
	log.Println("Extracting posts and pages...")
	posts, err := getPosts(ctx)
	if err != nil {
		log.Fatalf("Failed to retrieve posts: %v", err)
	}

	// Get unique authors
	authors, err := getAuthors(ctx, posts)
	if err != nil {
		log.Fatalf("Failed to retrieve authors: %v", err)
	}

	// Create channels and sync primitives
	postChan := make(chan Post, len(posts))
	resultChan := make(chan Post, len(posts))
	var wg sync.WaitGroup

	// Start workers
	log.Printf("Fetching content for %d posts (this may take a moment)...", len(posts))
	for range maxWorkers {
		wg.Add(1)
		go worker(ctx, &wg, postChan, resultChan, aiProvider)
	}

	// Distribute work
	for _, p := range posts {
		if author, ok := authors[p.AuthorID]; ok {
			p.Author = author
		}
		postChan <- p
	}
	close(postChan)

	// Collect results
	var combinedData []Post
	resultWg := &sync.WaitGroup{}
	resultWg.Add(1)
	go func() {
		defer resultWg.Done()
		for post := range resultChan {
			combinedData = append(combinedData, post)
		}
	}()

	wg.Wait()
	close(resultChan)
	resultWg.Wait()

	// Write to CSV
	metaColumns := buildMetaColumns(combinedData)
	if err := writeCSVHeader(csvWriter, metaColumns); err != nil {
		log.Fatalf("Error writing CSV headers: %v", err)
	}
	writeCSV(csvWriter, combinedData, metaColumns)
	log.Printf("Processing complete! Wrote %d rows to %s", len(combinedData), outputCSVPath)
}

func parseCSVArg(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	values := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		if strings.Contains(v, " ") {
			return nil, fmt.Errorf("value '%s' contains whitespace; use comma-separated values", v)
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		values = append(values, v)
	}
	return values, nil
}

func runWPCommand(ctx context.Context, command []string) (string, error) {
	const maxAttempts = 3
	backoff := 500 * time.Millisecond
	wpArgs := append([]string{"--allow-root", "--skip-plugins", "--skip-themes"}, command...)
	var lastErr error
	var lastStderr string

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		fullCmd := append([]string{"exec", "-u", "0", dockerContainer, "wp"}, wpArgs...)
		cmd := exec.CommandContext(ctx, "docker", fullCmd...)
		var out bytes.Buffer
		var stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err == nil {
			return out.String(), nil
		}
		lastErr = err
		lastStderr = stderr.String()
		if attempt == maxAttempts || !shouldRetryWPCommand(err) {
			return "", fmt.Errorf("command failed: %w. Stderr: %s", err, lastStderr)
		}
		log.Printf("WP-CLI command failed (attempt %d/%d): %v. Retrying in %s...", attempt, maxAttempts, err, backoff)
		select {
		case <-time.After(backoff):
			backoff *= 2
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	return "", fmt.Errorf("command failed: %w. Stderr: %s", lastErr, lastStderr)
}

func shouldRetryWPCommand(err error) bool {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode() == 137
	}
	return false
}

func getPosts(ctx context.Context) ([]Post, error) {
	fields := "ID,post_title,post_author,post_date,post_type,guid"
	if len(postTypes) == 0 {
		return nil, fmt.Errorf("no post types configured")
	}
	cmd := []string{"post", "list", fmt.Sprintf("--post_type=%s", strings.Join(postTypes, ",")), fmt.Sprintf("--fields=%s", fields), "--format=json"}
	output, err := runWPCommand(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var posts []Post
	if err := json.Unmarshal([]byte(output), &posts); err != nil {
		return nil, err
	}
	return posts, nil
}

func getAuthors(ctx context.Context, posts []Post) (map[string]Author, error) {
	authorIDs := make(map[string]struct{})
	for _, p := range posts {
		authorIDs[p.AuthorID] = struct{}{}
	}

	authorsData := make(map[string]Author)
	log.Printf("Found %d unique authors. Fetching their data...", len(authorIDs))
	for id := range authorIDs {
		fields := "ID,display_name,user_email,user_login,roles"
		cmd := []string{"user", "get", id, fmt.Sprintf("--fields=%s", fields), "--format=json"}
		output, err := runWPCommand(ctx, cmd)
		if err != nil {
			log.Printf("Warning: could not fetch author %s: %v", id, err)
			continue
		}
		var author Author
		if err := json.Unmarshal([]byte(output), &author); err != nil {
			log.Printf("Warning: could not parse author data for ID %s: %v", id, err)
			continue
		}
		authorsData[id] = author
	}
	return authorsData, nil
}

func worker(ctx context.Context, wg *sync.WaitGroup, postChan <-chan Post, resultChan chan<- Post, aiProvider string) {
	defer wg.Done()
	for post := range postChan {
		post.Meta = map[string]string{}

		// Fetch content
		content := ""
		content, err := runWPCommand(ctx, []string{"post", "get", strconv.Itoa(post.ID), "--field=content"})
		if err != nil {
			log.Printf("Error fetching content for post %d: %v", post.ID, err)
		} else {
			content = strings.TrimSpace(content)
			if len(content) > 300 {
				post.ContentExcerpt = content[:300] + "..."
			} else {
				post.ContentExcerpt = content
			}
		}

		if scanAllMeta || len(metaKeys) > 0 {
			meta, err := getPostMeta(ctx, post.ID)
			if err != nil {
				log.Printf("Error fetching meta for post %d: %v", post.ID, err)
			} else {
				post.Meta = filterPostMeta(meta)
			}
		}

		// Analyze content if enabled
		post.AIClassification = "N/A"
		post.AIJustification = "N/A"
		analysisInput := buildAIInput(content, post.Meta)
		if analyzeContent && analysisInput != "" {
			log.Printf("Analyzing content for post ID: %d...", post.ID)
			var apiKey string
			switch aiProvider {
			case "ollama":
				apiKey = os.Getenv("OLLAMA_API_KEY")
			case "google":
				apiKey = os.Getenv("GEMINI_API_KEY")
			}
			aiResult, err := analyzeContentViaAI(ctx, aiProvider, apiKey, promptText, analysisInput)
			if err != nil {
				log.Printf("Error analyzing post %d: %v", post.ID, err)
				post.AIClassification = "Error"
				post.AIJustification = err.Error()
			} else {
				post.AIClassification = aiResult.Classification
				post.AIJustification = aiResult.Justification
			}
			time.Sleep(1 * time.Second) // Avoid hitting API rate limits
		}
		resultChan <- post
	}
}

func getPostMeta(ctx context.Context, postID int) (map[string]string, error) {
	output, err := runWPCommand(ctx, []string{"post", "meta", "list", strconv.Itoa(postID), "--format=json"})
	if err != nil {
		return nil, err
	}
	output = strings.TrimSpace(output)
	if output == "" {
		return map[string]string{}, nil
	}

	var entries []PostMetaEntry
	if err := json.Unmarshal([]byte(output), &entries); err != nil {
		return nil, err
	}

	meta := make(map[string]string, len(entries))
	for _, entry := range entries {
		key := strings.TrimSpace(entry.Key)
		if key == "" {
			continue
		}
		val := strings.TrimSpace(entry.Value)
		if val == "" {
			continue
		}
		if existing, ok := meta[key]; ok {
			meta[key] = existing + " | " + val
			continue
		}
		meta[key] = val
	}

	return meta, nil
}

func filterPostMeta(all map[string]string) map[string]string {
	if len(all) == 0 {
		return map[string]string{}
	}
	if scanAllMeta {
		out := make(map[string]string, len(all))
		maps.Copy(out, all)
		return out
	}

	if len(metaKeysSet) == 0 {
		return map[string]string{}
	}

	out := make(map[string]string, len(metaKeysSet))
	for key := range metaKeysSet {
		if val, ok := all[key]; ok {
			out[key] = val
		}
	}
	return out
}

func buildAIInput(content string, meta map[string]string) string {
	content = strings.TrimSpace(content)
	hasMeta := len(meta) > 0
	if content == "" && !hasMeta {
		return ""
	}

	parts := make([]string, 0, 2)
	if content != "" {
		parts = append(parts, "POST CONTENT:\n"+content)
	}
	if hasMeta {
		keys := make([]string, 0, len(meta))
		for k := range meta {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		metaLines := make([]string, 0, len(keys))
		for _, k := range keys {
			metaLines = append(metaLines, fmt.Sprintf("%s: %s", k, meta[k]))
		}
		parts = append(parts, "POST META:\n"+strings.Join(metaLines, "\n"))
	}

	return strings.Join(parts, "\n\n---\n\n")
}

func analyzeContentViaAI(ctx context.Context, provider string, apiKey string, prompt string, content string) (*AIResult, error) {
	fullPrompt := fmt.Sprintf("%s\n\n---\n\nCONTENT TO ANALYZE:\n%s", prompt, content)

	var rawJSON string
	// var err error

	switch provider {
	case "ollama":
		// Prepare the request payload for Ollama Cloud
		payload := map[string]any{
			"model": "gpt-oss:120b", // or another model supported by Ollama Cloud
			"messages": []map[string]string{
				{
					"role":    "user",
					"content": fullPrompt,
				},
			},
			"stream": false,
		}

		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal payload: %w", err)
		}

		// Send the request to Ollama Cloud
		ollamaURL := "https://ollama.com/api/chat"
		req, err := http.NewRequestWithContext(ctx, "POST", ollamaURL, bytes.NewBuffer(payloadBytes))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to send request to Ollama Cloud: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Ollama Cloud API returned non-200 status: %d", resp.StatusCode)
		}

		// Parse the response
		var result map[string]any{}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("failed to decode Ollama Cloud response: %w", err)
		}

		message, ok := result["message"].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("Ollama Cloud response does not contain 'message' field")
		}

		rawJSON, ok = message["content"].(string)
		if !ok {
			return nil, fmt.Errorf("Ollama Cloud response does not contain 'content' field")
		}

	case "google":
		// Initialize Google GenAI client
		client, err := genai.NewClient(ctx, &genai.ClientConfig{
			APIKey: apiKey,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create Google GenAI client: %w", err)
		}

		// Generate content using Google GenAI
		result, err := client.Models.GenerateContent(
			ctx,
			"gemini-3-flash-preview", // or another model
			genai.Text(fullPrompt),
			nil,
		)
		if err != nil {
			return nil, fmt.Errorf("Google GenAI generation failed: %w", err)
		}

		rawJSON = result.Text()
		if rawJSON == "" {
			return nil, fmt.Errorf("Google GenAI response is empty")
		}

	default:
		return nil, fmt.Errorf("unsupported AI provider: %s", provider)
	}

	// Clean and parse the JSON response
	cleanedJSON := strings.Trim(rawJSON, " \n\t`")
	if after, ok := strings.CutPrefix(cleanedJSON, "json"); ok {
		cleanedJSON = after
	}
	cleanedJSON = strings.Trim(cleanedJSON, " \n\t`")

	var aiResult AIResult
	if err := json.Unmarshal([]byte(cleanedJSON), &aiResult); err != nil {
		return nil, fmt.Errorf("failed to decode AI JSON response: %w. Raw: %s", err, rawJSON)
	}

	if aiResult.Classification == "" || aiResult.Justification == "" {
		return nil, fmt.Errorf("AI response has incorrect format. Raw: %s", rawJSON)
	}

	return &aiResult, nil
}

func defaultPrompt() string {
	return `Analyze the following content and provide insights on potential issues. The idea is to identify whether the content is spam or legitimate as it relates to the intent and purpose of the website. Classify the content as 'Spam', 'Legitimate', or 'Uncertain' and provide a brief justification for your choice. Return the classification and justification in valid JSON format like so: {"classification": "Spam", "justification": "..."}. Below is the about page description of the website to help you understand its purpose: We are a local service business dedicated to providing high-quality residential and commercial services to our community. Our team of experienced professionals is committed to customer satisfaction and reliable service. Please feel free to contact us for more information on our services, products, and company.`
}

func loadPromptFromFile(path string) (string, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	prompt := strings.TrimSpace(string(bytes))
	if prompt == "" {
		return "", fmt.Errorf("prompt file is empty")
	}
	return prompt, nil
}

func initializeCSV() (*os.File, *csv.Writer) {
	file, err := os.Create(outputCSVPath)
	if err != nil {
		log.Fatalf("Error creating CSV file %s: %v", outputCSVPath, err)
	}
	writer := csv.NewWriter(file)
	return file, writer
}

func buildMetaColumns(data []Post) []string {
	if !scanAllMeta {
		return append([]string(nil), metaKeys...)
	}

	set := make(map[string]struct{})
	for _, post := range data {
		for k := range post.Meta {
			if strings.TrimSpace(k) == "" {
				continue
			}
			set[k] = struct{}{}
		}
	}

	columns := make([]string, 0, len(set))
	for k := range set {
		columns = append(columns, k)
	}
	sort.Strings(columns)
	return columns
}

func writeCSVHeader(writer *csv.Writer, metaColumns []string) error {
	headers := []string{
		"post_id", "post_title", "post_type", "post_date", "post_guid",
		"content_excerpt", "author_id", "author_display_name", "author_email",
		"author_login", "ai_classification", "ai_justification",
	}
	for _, key := range metaColumns {
		headers = append(headers, "meta_"+key)
	}
	return writer.Write(headers)
}

func writeCSV(writer *csv.Writer, data []Post, metaColumns []string) {
	for _, post := range data {
		row := []string{
			strconv.Itoa(post.ID),
			post.Title,
			post.Type,
			post.Date,
			post.GUID,
			post.ContentExcerpt,
			post.AuthorID,
			post.Author.DisplayName,
			post.Author.Email,
			post.Author.Login,
			post.AIClassification,
			post.AIJustification,
		}
		for _, key := range metaColumns {
			row = append(row, post.Meta[key])
		}
		if err := writer.Write(row); err != nil {
			log.Printf("Error writing row to CSV for post %d: %v", post.ID, err)
		}
	}
}
