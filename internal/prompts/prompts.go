package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"text/template"
	"path/filepath"
	"sync"
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
	info, err := os.Stat(filename)
	var modTime time.Time
	if err == nil {
		modTime = info.ModTime()
	}

	tm.mu.RLock()
	tmpl, exists := tm.templates[name]
	lastMod, modExists := tm.lastModified[name]
	tm.mu.RUnlock()

	// If template exists and file on disk hasn't changed (or doesn't exist on disk), return cached
	if exists && (!modExists || modTime.IsZero() || modTime.Equal(lastMod)) {
		return tmpl, nil
	}

	// Needs reload or initial load
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Double-check after acquiring write lock
	tmpl, exists = tm.templates[name]
	lastMod, modExists = tm.lastModified[name]
	if exists && (!modExists || modTime.IsZero() || modTime.Equal(lastMod)) {
		return tmpl, nil
	}

	var content []byte
	if err == nil {
		// Read from disk
		content, err = os.ReadFile(filename)
		if err != nil {
			slog.Warn("Failed to read template from disk, falling back to embedded", "file", filename, "err", err)
			content, err = defaultTemplates.ReadFile(name)
		}
	} else {
		// Fallback to embedded
		slog.Debug("Template not found on disk, using embedded fallback", "file", filename)
		content, err = defaultTemplates.ReadFile(name)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to load template %s: %w", name, err)
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
