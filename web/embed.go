package web

import _ "embed"

// Embedded assets for the /manager tmux web dashboard and /login authentication page.
var (
	//go:embed manager.html
	ManagerHTML []byte

	//go:embed manager.css
	ManagerCSS []byte

	//go:embed manager.js
	ManagerJS []byte

	//go:embed login.html
	LoginHTML []byte
)
