package watermark

import (
	"image"
	"log"
	"os"
	"sync"
	"time"

	"github.com/disintegration/imaging"
)

type Manager struct {
	path        string
	opacity     float64
	currentImg  image.Image
	lastModTime time.Time
	mu          sync.RWMutex
	debug       bool
}

func NewManager(path string, opacity float64, debug bool) *Manager {
	return &Manager{
		path:    path,
		opacity: opacity,
		debug:   debug,
	}
}

func (m *Manager) Get() (image.Image, float64, error) {
	if m.path == "" {
		return nil, 0, nil
	}

	info, err := os.Stat(m.path)
	if err != nil {
		return nil, 0, err
	}

	m.mu.RLock()
	// If mod time hasn't changed and we have an image, return it
	if !info.ModTime().After(m.lastModTime) && m.currentImg != nil {
		defer m.mu.RUnlock()
		return m.currentImg, m.opacity, nil
	}
	m.mu.RUnlock()

	// Upgrade lock to write
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double check
	if !info.ModTime().After(m.lastModTime) && m.currentImg != nil {
		return m.currentImg, m.opacity, nil
	}

	if m.debug {
		log.Printf("Loading watermark from %s", m.path)
	}

	img, err := imaging.Open(m.path)
	if err != nil {
		return nil, 0, err
	}

	m.currentImg = img
	m.lastModTime = info.ModTime()

	return m.currentImg, m.opacity, nil
}
