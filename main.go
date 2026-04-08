package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/afkarxyz/SpotiFLAC/backend"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed wails.json
var wailsJSON []byte

func main() {
	// Parse command line flags
	serverMode := flag.Bool("server", false, "Run in HTTP server mode (headless)")
	port := flag.String("port", "8787", "HTTP server port (only used with -server)")
	outputDir := flag.String("output", "/data/downloads/spotiflac", "Download output directory (only used with -server)")
	service := flag.String("service", "amazon", "Default music service: amazon, tidal, qobuz (only used with -server)")
	quality := flag.String("quality", "LOSSLESS", "Default audio quality (only used with -server)")
	embedLyrics := flag.Bool("embed-lyrics", true, "Embed lyrics in audio files (only used with -server)")
	embedCover := flag.Bool("embed-cover", true, "Embed high quality cover art (only used with -server)")
	filenameFormat := flag.String("filename-format", "title-artist", "Filename format (only used with -server)")
	flag.Parse()

	// Set app version from wails.json
	type wailsInfo struct {
		Info struct {
			ProductVersion string `json:"productVersion"`
		} `json:"info"`
	}
	var config wailsInfo
	if err := json.Unmarshal(wailsJSON, &config); err == nil && config.Info.ProductVersion != "" {
		backend.AppVersion = config.Info.ProductVersion
	}

	if *serverMode {
		// Run in HTTP server mode (headless)
		runServerMode(*port, *outputDir, *service, *quality, *embedLyrics, *embedCover, *filenameFormat)
	} else {
		// Run in Wails desktop app mode
		runWailsApp()
	}
}

// runServerMode starts the HTTP server for headless operation
func runServerMode(port, outputDir, service, quality string, embedLyrics, embedCover bool, filenameFormat string) {
	log.Println("Starting SpotiFLAC in HTTP server mode...")

	// Create output directory if it doesn't exist
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory %s: %v", outputDir, err)
	}

	serverConfig := ServerConfig{
		Port:           port,
		OutputDir:      outputDir,
		Service:        service,
		Quality:        quality,
		EmbedLyrics:    embedLyrics,
		EmbedCover:     embedCover,
		FilenameFormat: filenameFormat,
	}

	server := NewServer(serverConfig)

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down server...")
		ctx, cancel := context.WithTimeout(context.Background(), 10)
		defer cancel()
		server.Shutdown(ctx)
		os.Exit(0)
	}()

	if err := server.Start(); err != nil {
		log.Fatal("Server error:", err)
	}
}

// runWailsApp starts the Wails desktop application
func runWailsApp() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "SpotiFLAC",
		Width:     1024,
		Height:    600,
		MinWidth:  1024,
		MinHeight: 600,
		Frameless: true,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 0, G: 0, B: 0, A: 255},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		DragAndDrop: &options.DragAndDrop{
			EnableFileDrop:     true,
			DisableWebViewDrop: false,
			CSSDropProperty:    "--wails-drop-target",
			CSSDropValue:       "drop",
		},
		Bind: []interface{}{
			app,
		},
		Windows: &windows.Options{
			WebviewIsTransparent:              false,
			WindowIsTranslucent:               false,
			DisableWindowIcon:                 false,
			DisableFramelessWindowDecorations: false,
		},
	})

	if err != nil {
		log.Fatal("Error:", err.Error())
	}
}
