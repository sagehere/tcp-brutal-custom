package main

import _ "embed"

//go:embed web/index.html
var panelHTML []byte

//go:embed web/app.js
var panelJS []byte
