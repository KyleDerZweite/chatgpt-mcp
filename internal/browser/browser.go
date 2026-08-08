package browser

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"

	"chatgpt-mcp/internal/config"
)

const (
	ChatURL      = "https://chatgpt.com"
	ModelURLBase = ChatURL + "/?model="
	loginMaxWait = 5 * time.Minute
)

var PromptTextareaSelectors = []string{
	"#prompt-textarea",
	`[data-testid="prompt-textarea"]`,
	`textarea[placeholder*="Message"]`,
	`.ProseMirror`,
	`div[contenteditable="true"]`,
}

var loginButtonSelectors = []string{
	`button[data-testid="login-button"]`,
	`a[href*="auth/login"]`,
	`button:has-text("Log in")`,
}

type Session struct {
	cfg  *config.Config
	rod  *rod.Browser
	Page *rod.Page
}

func New(cfg *config.Config) *Session {
	return &Session{cfg: cfg}
}

func (s *Session) Ensure() error {
	if s.Page != nil {
		return nil
	}

	var err error
	if s.cfg.CDPURL != "" {
		err = s.attach()
	} else {
		err = s.launch()
	}
	if err != nil {
		return err
	}

	pages, err := s.rod.Pages()
	if err != nil {
		return fmt.Errorf("list pages: %w", err)
	}
	for _, p := range pages {
		info, err := p.Info()
		if err == nil && strings.Contains(info.URL, "chatgpt.com") {
			s.Page = p
			return nil
		}
	}

	page, err := s.rod.Page(proto.TargetCreateTarget{URL: ChatURL})
	if err != nil {
		return fmt.Errorf("new page: %w", err)
	}
	if err := page.WaitLoad(); err != nil {
		return fmt.Errorf("load chatgpt: %w", err)
	}
	s.Page = page
	return nil
}

func (s *Session) launch() error {
	l := launcher.New().
		UserDataDir(s.cfg.ProfileDir).
		Headless(s.cfg.Headless).
		Set("disable-blink-features", "AutomationControlled").
		Delete("enable-automation")
	if s.cfg.ChromeBin != "" {
		l.Bin(s.cfg.ChromeBin)
	}

	ctrlURL, err := l.Launch()
	if err != nil {
		return fmt.Errorf("start chrome with profile %s: %w (log into ChatGPT in this profile to keep the session)", s.cfg.ProfileDir, err)
	}

	r := rod.New().ControlURL(ctrlURL)
	if err := r.Connect(); err != nil {
		return fmt.Errorf("connect to chrome: %w", err)
	}
	s.rod = r
	return nil
}

func (s *Session) attach() error {
	r := rod.New().ControlURL(s.cfg.CDPURL)
	if err := r.Connect(); err != nil {
		return fmt.Errorf("attach to chrome at %s: %w (start chrome with --remote-debugging-port=9222 --user-data-dir=%s and log into ChatGPT)", s.cfg.CDPURL, err, s.cfg.ProfileDir)
	}
	s.rod = r
	return nil
}

func (s *Session) Navigate(url string) error {
	if err := s.Page.Navigate(url); err != nil {
		return fmt.Errorf("navigate to %s: %w", url, err)
	}
	return s.Page.WaitLoad()
}

func (s *Session) Ready() bool {
	for _, sel := range PromptTextareaSelectors {
		if found, _, err := s.Page.Has(sel); err == nil && found {
			return true
		}
	}
	return false
}

func (s *Session) NeedsLogin() bool {
	for _, sel := range loginButtonSelectors {
		if found, _, err := s.Page.Has(sel); err == nil && found {
			return true
		}
	}
	return false
}

func (s *Session) WaitReady(max time.Duration) error {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if s.Ready() {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	if s.NeedsLogin() {
		return fmt.Errorf("ChatGPT shows a login prompt; log in using the browser window and retry")
	}
	return fmt.Errorf("chat interface did not become ready within %s", max)
}

func (s *Session) DebugShot(name string) {
	if !s.cfg.Screenshots || s.Page == nil {
		return
	}
	data, err := s.Page.Screenshot(false, nil)
	if err != nil {
		return
	}
	path := fmt.Sprintf("%s/%s-%d.png", s.cfg.DebugDir, name, time.Now().Unix())
	_ = os.WriteFile(path, data, 0o644)
}
