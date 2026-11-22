package main

import (
	_ "embed"
	"strings"
)

//go:embed web/page.html
var pageHTML string

//go:embed web/page.css
var pageCSS string

//go:embed web/page.js
var pageJS string

func pluginPage() []byte {
	page := strings.Replace(pageHTML, "/*CSS*/", pageCSS, 1)
	page = strings.Replace(page, "/*JS*/", pageJS, 1)
	return []byte(page)
}
