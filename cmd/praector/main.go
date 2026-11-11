package main

import (
	"github.com/datascape/praector/internal/praector"
	"os"
)

func main() { os.Exit(praector.Main(os.Args[1:])) }
