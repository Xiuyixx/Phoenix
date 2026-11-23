package main

import (
	_ "embed"
	"encoding/base64"
	"strings"
)

//go:embed web/page.html
var pageHTML string

//go:embed web/page.css
var pageCSS string

//go:embed web/page.js
var pageJS string

//go:embed web/logo.jpg
var logoJPEG []byte

func pluginPage() []byte {
	page := strings.Replace(pageHTML, "/*CSS*/", pageCSS, 1)
	page = strings.Replace(page, "/*JS*/", pageJS, 1)
	page = strings.Replace(page, "/*LOGO*/", "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(logoJPEG), 1)
	return []byte(page)
}
