package main

import "embed"

//go:embed dist/*
var embeddedDist embed.FS
