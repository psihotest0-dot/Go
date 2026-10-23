package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"
)

//go:embed *.tmpl
var defaultTemplates embed.FS

// GlobalManager is the globally accessible instance.
var GlobalManager *TemplateManager

// Init initializes the global template manager.
func Init(basePath string) {
	GlobalManager = NewTemplateManager(basePath)
}

// TemplateManager manages loading and rendering of templates.
type TemplateManager struct {
	mu           sync.RWMutex
	basePath     string
	templates    map[string]*template.Template
	lastModified map[string]time.Time
}

// NewTemplateManager creates a new TemplateManager.
func NewTemplateManager(basePath string) *TemplateManager {
	return &TemplateManager{
		basePath:     basePath,
		templates:    make(map[string]*template.Template),
		lastModified: make(map[string]time.Time),
	}
}

// Preload loads the specified templates, returning an error if any fail to load.
func (tm *TemplateManager) Preload(names ...string) error {
	for _, name := range names {
		if _, err := tm.getTemplate(name); err != nil {
			return err
		}
	}
	return nil
}

// Render executes the given template by name.
func (tm *TemplateManager) Render(name string, data interface{}) (string, error) {
	tmpl, err := tm.getTemplate(name)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to render template %s: %w", name, err)
	}

	return buf.String(), nil
}

func (tm *TemplateManager) getTemplate(name string) (*template.Template, error) {
	filename := filepath.Join(tm.basePath, name)

	// Check if file has been modified on disk
	info, statErr := os.Stat(filename)
	var modTime time.Time
	if statErr == nil {
		modTime = info.ModTime()
	}

	tm.mu.RLock()
	tmpl, exists := tm.templates[name]
	lastMod, modExists := tm.lastModified[name]
	tm.mu.RUnlock()

	// If template exists and file on disk hasn't changed (or doesn't exist on disk), return cached
	if exists && modExists && modTime.Equal(lastMod) {
		return tmpl, nil
	}

	// Needs reload or initial load
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Double-check after acquiring write lock
	tmpl, exists = tm.templates[name]
	lastMod, modExists = tm.lastModified[name]
	if exists && modExists && modTime.Equal(lastMod) {
		return tmpl, nil
	}

	normalizedName := strings.ReplaceAll(name, "\\", "/")
	var content []byte
	var loadErr error

	if statErr == nil {
		// Read from disk
		var readErr error
		content, readErr = os.ReadFile(filename)
		if readErr != nil {
			slog.Warn("Failed to read template from disk, falling back to embedded", "file", filename, "err", readErr)
			content, loadErr = defaultTemplates.ReadFile(normalizedName)
		}
	} else {
		// Fallback to embedded
		slog.Debug("Template not found on disk, using embedded fallback", "file", filename)
		content, loadErr = defaultTemplates.ReadFile(normalizedName)
	}

	if loadErr != nil {
		return nil, fmt.Errorf("failed to load template %s: %w", name, loadErr)
	}

	newTmpl, err := template.New(name).Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("failed to parse template %s: %w", name, err)
	}

	tm.templates[name] = newTmpl
	if !modTime.IsZero() {
		tm.lastModified[name] = modTime
	} else {
		// If loaded from embed, set a zero time or omit from lastModified
		tm.lastModified[name] = time.Time{}
	}

	return newTmpl, nil
}
