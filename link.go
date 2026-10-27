package main

import (
	"fmt"
	"net/url"

	"github.com/cli/browser"
)

// A link is one URL per unit, which is what a pull request is. Generic on
// purpose: GitHub, GitLab, Forgejo, a review doc, a dashboard. The tool never
// parses it, so no forge gets special treatment.

// checkLink refuses anything that is not http or https. A view that opens
// whatever string a plan or a lane wrote would happily launch file:// or worse.
func checkLink(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("link needs a url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%q is not a url: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%q must be http or https, not %q", raw, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%q has no host", raw)
	}
	return u.String(), nil
}

// SetLink records the url, or clears it. The empty string clears.
func (s *State) SetLink(unit, raw string) error {
	u := s.Units[unit]
	if raw == "" {
		u.Link = ""
		s.Units[unit] = u
		return nil
	}
	link, err := checkLink(raw)
	if err != nil {
		return err
	}
	u.Link = link
	if u.At == "" {
		u.At = now()
	}
	s.Units[unit] = u
	return nil
}

// openURL hands the url to the desktop through cli/browser, which knows the
// opener on linux, macOS and Windows. The view never fetches anything itself:
// the browser is the right place for a pull request.
func openURL(raw string) error {
	if _, err := checkLink(raw); err != nil {
		return err
	}
	return browser.OpenURL(raw)
}

// linkMark is the row's hint that a unit has a link, without moving any column
// that another row depends on.
func linkMark(u UnitRow) string {
	if u.Link != "" {
		return "↗ "
	}
	return ""
}
