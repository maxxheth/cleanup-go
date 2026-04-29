# wp-content-cleanup

A Go CLI tool that extracts posts, pages, and custom post types from a WordPress site running inside a Docker container, exports the data to CSV, and optionally classifies content as spam or legitimate using AI (Google Gemini or Ollama Cloud).

## Features

- **WordPress Data Extraction** — Queries WP-CLI inside a Docker container to pull post data, author info, and post meta.
- **Configurable Post Types** — Scan any combination of WordPress post types (e.g. `post`, `page`, `product`).
- **Selective Meta Scanning** — Choose specific meta keys to extract, or scan all meta keys at once with `--scan-all-meta`.
- **AI-Powered Spam Detection** — Sends post content and meta to an AI provider for classification as "Spam", "Legitimate", or "Uncertain" with a justification.
- **Concurrent Workers** — Fetches and analyzes content in parallel (default: 10 workers) for faster processing.
- **Retry with Backoff** — Automatically retries WP-CLI commands that fail due to container restarts (exit code 137), up to 3 attempts with exponential backoff.
- **CSV Export** — Writes all extracted and analyzed data to a CSV file with dynamic meta columns.

## Prerequisites

- **Go 1.23+** (or use the pre-built binary)
- **Docker** — A running Docker container with WordPress and WP-CLI installed
- **AI API Key** (optional — only if using AI analysis):
  - Google Gemini: `GEMINI_API_KEY`
  - Ollama Cloud: `OLLAMA_API_KEY`

## Installation

### From Source

```bash
git clone <repo-url> cleanup-go
cd cleanup-go
go build -o cleanup-go .
```

### Using the Pre-built Binary

```bash
chmod +x cleanup-go
./cleanup-go --help
```

## Configuration

### Environment Variables

Create a `.env` file in the project root (or set them in your shell):

| Variable | Required | Description |
|---|---|---|
| `GEMINI_API_KEY` | If using `--ai-provider google` | Your Google Gemini API key |
| `OLLAMA_API_KEY` | If using `--ai-provider ollama` | Your Ollama Cloud API key |
| `WP_CLEANUP_PROMPT_FILE` | No | Default path to a prompt file (overridden by `--prompt-file`) |

### AI Prompt File

You can customize the AI analysis prompt by creating a text file. The prompt instructs the AI to classify content and return JSON. A sample is provided at `prompt.sample.txt`:

```
Analyze the following content and provide insights on potential issues. The idea is to identify whether the content is spam or legitimate as it relates to the intent and purpose of the website.

Classify the content as "Spam", "Legitimate", or "Uncertain" and provide a brief justification for your choice. Return the classification and justification in valid JSON format like so: {"classification": "Spam", "justification": "..."}.

Below is the about page description of the website to help you understand its purpose:
<your website description here>
```

The prompt file should end with a description of the website so the AI can judge whether content aligns with the site's purpose.

## Usage

```bash
./cleanup-go [flags]
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--container-name` | `wordpress` | Name of the Docker container running WordPress |
| `--output-csv-path` | `wp_content.csv` | Path for the output CSV file |
| `--analyze-post-content-via-ai` | `false` | Enable AI analysis of post content |
| `--ai-provider` | `ollama` | AI provider to use: `ollama` or `google` |
| `--prompt-file` | `""` | Path to a text file containing the AI prompt (overrides `WP_CLEANUP_PROMPT_FILE` env var) |
| `--post-types` | `post,page` | Comma-separated list of WordPress post types to scan |
| `--meta-keys` | `""` | Comma-separated list of meta keys to extract (e.g. `_yoast_wpseo_metadesc,custom_summary`) |
| `--scan-all-meta` | `false` | Extract all meta keys for the selected post types (overrides `--meta-keys`) |

### Examples

**Basic extraction (no AI):**

```bash
./cleanup-go --container-name my_wp_site
```

**Extract posts, pages, and WooCommerce products with specific meta keys:**

```bash
./cleanup-go \
  --container-name my_wp_site \
  --post-types "post,page,product" \
  --meta-keys "_yoast_wpseo_metadesc,custom_summary" \
  --output-csv-path /tmp/export.csv
```

**Full extraction with Google Gemini AI analysis:**

```bash
./cleanup-go \
  --container-name my_wp_site \
  --analyze-post-content-via-ai \
  --ai-provider google \
  --prompt-file ./prompt.sample.txt \
  --post-types "post,page" \
  --output-csv-path /tmp/spam-results.csv
```

**Using the provided run script:**

```bash
# Edit run.sh to match your container name and preferences, then:
bash run.sh
```

## How It Works

```
┌──────────────┐     ┌──────────────────┐     ┌────────────┐
│  Docker CLI  │────▶│  WP-CLI Commands │────▶│ WordPress   │
└──────────────┘     └──────────────────┘     └────────────┘
       │
       ▼
┌──────────────────────────────────────────────────┐
│              Worker Pool (10 concurrent)          │
│                                                  │
│  For each post:                                  │
│  1. Fetch post content via WP-CLI                │
│  2. Fetch & filter post meta via WP-CLI          │
│  3. (Optional) Send content + meta to AI API     │
│  4. Emit result to output channel               │
└──────────────────────────────────────────────────┘
       │
       ▼
┌──────────────────────────────────┐
│         CSV Writer               │
│                                  │
│  Headers: post_id, post_title,   │
│  post_type, post_date, ...,     │
│  meta_<key>, ai_classification, │
│  ai_justification               │
└──────────────────────────────────┘
```

1. **Container Check** — Verifies the Docker container is running via `docker inspect`.
2. **Post Discovery** — Runs `wp post list` for the configured post types, retrieving IDs, titles, authors, dates, types, and GUIDs.
3. **Author Resolution** — Fetches unique author details (display name, email, login, roles) via `wp user get`.
4. **Concurrent Processing** — Distributes posts across a worker pool. Each worker:
   - Fetches the full post content (`wp post get --field=content`), truncating to 300 characters for the excerpt.
   - Optionally fetches and filters post meta (`wp post meta list`).
   - Optionally sends the content and meta to the chosen AI provider for spam classification.
   - Includes a 1-second delay between AI calls to respect rate limits.
5. **CSV Export** — Collects all results and writes a single CSV with static columns plus dynamic `meta_<key>` columns.

## AI Providers

### Google Gemini

Set `--ai-provider google` and provide a `GEMINI_API_KEY`. The tool uses the `google.golang.org/genai` SDK to call the Gemini model and expects a JSON response matching the `AIResult` schema.

### Ollama Cloud

Set `--ai-provider ollama` (the default) and provide an `OLLAMA_API_KEY`. The tool sends a chat completion request to the Ollama Cloud API (`https://ollama.com/api/chat`) using the `gpt-oss:120b` model.

Both providers are expected to return JSON in this format:

```json
{
  "classification": "Spam",
  "justification": "Content contains unrelated promotional links..."
}
```

The tool strips markdown code fences and leading `json` labels from the response before parsing.

## Output

The CSV file contains the following columns:

| Column | Description |
|---|---|
| `post_id` | WordPress post ID |
| `post_title` | Post title |
| `post_type` | Post type (post, page, product, etc.) |
| `post_date` | Publication date |
| `post_guid` | Post GUID/URL |
| `content_excerpt` | First 300 characters of post content |
| `author_id` | Author user ID |
| `author_display_name` | Author display name |
| `author_email` | Author email |
| `author_login` | Author login name |
| `ai_classification` | Spam / Legitimate / Uncertain / N/A / Error |
| `ai_justification` | AI-provided reasoning for the classification |
| `meta_<key>` | One column per discovered or requested meta key |

## Project Structure

```
.
├── main.go              # Entry point — calls cmd.Execute()
├── cmd/
│   └── root.go          # All logic: CLI flags, WP-CLI interaction, AI, CSV
├── go.mod               # Go module definition
├── go.sum               # Dependency checksums
├── .env                 # Environment variables (not committed)
├── .gitignore
├── prompt.sample.txt    # Example AI prompt file
├── run.sh               # Example run script
└── wp_content.csv       # Example output (gitignored)
```

## Dependencies

| Package | Purpose |
|---|---|
| [github.com/spf13/cobra](https://github.com/spf13/cobra) | CLI framework with flags and subcommands |
| [github.com/joho/godotenv](https://github.com/joho/godotenv) | Load `.env` files for API keys |
| [google.golang.org/genai](https://pkg.go.dev/google.golang.org/genai) | Google Gemini AI SDK |

## Limitations

- Requires WordPress to be running inside a Docker container with WP-CLI available.
- Content excerpts are truncated to 300 characters in the CSV.
- AI rate-limiting is handled by a simple 1-second sleep between calls — for very large sites, you may want to adjust `maxWorkers` or add longer delays.
- The Ollama Cloud model name (`gpt-oss:120b`) and endpoint are hardcoded — modify `analyzeContentViaAI` in `cmd/root.go` if you need a different model or self-hosted Ollama instance.

## License

See [LICENSE](LICENSE) for details.