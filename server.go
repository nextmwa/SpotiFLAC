package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/afkarxyz/SpotiFLAC/backend"
)

// ServerConfig holds the configuration for the HTTP server
type ServerConfig struct {
	Port           string
	OutputDir      string
	Service        string // "amazon", "tidal", "qobuz"
	Quality        string // "LOSSLESS", "HI_RES_LOSSLESS", etc.
	EmbedLyrics    bool
	EmbedCover     bool
	FilenameFormat string
}

// Server is the HTTP server for SpotiFLAC
type Server struct {
	config     ServerConfig
	jobQueue   chan ImportJob
	jobs       map[string]*ImportJob
	jobsMutex  sync.RWMutex
	httpServer *http.Server
}

// ImportRequest is the request body for POST /api/import
type ImportRequest struct {
	URL         string `json:"url"`
	Service     string `json:"service,omitempty"`
	Quality     string `json:"quality,omitempty"`
	EmbedLyrics *bool  `json:"embed_lyrics,omitempty"`
	EmbedCover  *bool  `json:"embed_cover,omitempty"`
}

// ImportResponse is the response for POST /api/import
type ImportResponse struct {
	Success      bool   `json:"success"`
	JobID        string `json:"job_id,omitempty"`
	TracksQueued int    `json:"tracks_queued,omitempty"`
	Message      string `json:"message"`
	Error        string `json:"error,omitempty"`
}

// ImportJob represents a download job
type ImportJob struct {
	ID           string    `json:"id"`
	URL          string    `json:"url"`
	Type         string    `json:"type"` // "track", "album", "playlist"
	Name         string    `json:"name"`
	Status       string    `json:"status"` // "queued", "processing", "completed", "failed"
	TracksTotal  int       `json:"tracks_total"`
	TracksQueued int       `json:"tracks_queued"`
	CreatedAt    time.Time `json:"created_at"`
	Error        string    `json:"error,omitempty"`
}

// QueueResponse is the response for GET /api/queue
type QueueResponse struct {
	Items     []backend.DownloadItem `json:"items"`
	Total     int                    `json:"total"`
	Completed int                    `json:"completed"`
	Failed    int                    `json:"failed"`
	Queued    int                    `json:"queued"`
	Skipped   int                    `json:"skipped"`
}

// HealthResponse is the response for GET /api/health
type HealthResponse struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	Service   string `json:"service"`
	OutputDir string `json:"output_dir"`
}

// ProgressResponse is the response for GET /api/progress
type ProgressResponse struct {
	IsDownloading bool    `json:"is_downloading"`
	CurrentSpeed  float64 `json:"current_speed_mbps"`
	MBDownloaded  float64 `json:"mb_downloaded"`
}

// NewServer creates a new HTTP server
func NewServer(config ServerConfig) *Server {
	// Initialize history database
	if err := backend.InitHistoryDB("SpotiFLAC"); err != nil {
		log.Printf("Warning: Failed to init history DB: %v\n", err)
	}

	return &Server{
		config:   config,
		jobQueue: make(chan ImportJob, 100),
		jobs:     make(map[string]*ImportJob),
	}
}

// Start starts the HTTP server
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Register handlers
	mux.HandleFunc("/api/health", s.corsMiddleware(s.healthHandler))
	mux.HandleFunc("/api/import", s.corsMiddleware(s.importHandler))
	mux.HandleFunc("/api/queue", s.corsMiddleware(s.queueHandler))
	mux.HandleFunc("/api/progress", s.corsMiddleware(s.progressHandler))
	mux.HandleFunc("/api/history", s.corsMiddleware(s.historyHandler))
	mux.HandleFunc("/api/jobs", s.corsMiddleware(s.jobsHandler))

	s.httpServer = &http.Server{
		Addr:    ":" + s.config.Port,
		Handler: mux,
	}

	// Start job processor
	go s.processJobs()

	log.Printf("SpotiFLAC HTTP server starting on :%s", s.config.Port)
	log.Printf("  Output directory: %s", s.config.OutputDir)
	log.Printf("  Default service: %s", s.config.Service)
	log.Printf("  Default quality: %s", s.config.Quality)

	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	backend.CloseHistoryDB()
	return s.httpServer.Shutdown(ctx)
}

// corsMiddleware adds CORS headers to responses
func (s *Server) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
}

// healthHandler handles GET /api/health
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := HealthResponse{
		Status:    "ok",
		Version:   backend.AppVersion,
		Service:   s.config.Service,
		OutputDir: s.config.OutputDir,
	}

	s.jsonResponse(w, http.StatusOK, resp)
}

// importHandler handles POST /api/import
func (s *Server) importHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.jsonResponse(w, http.StatusBadRequest, ImportResponse{
			Success: false,
			Error:   "Invalid JSON: " + err.Error(),
		})
		return
	}

	if req.URL == "" {
		s.jsonResponse(w, http.StatusBadRequest, ImportResponse{
			Success: false,
			Error:   "URL is required",
		})
		return
	}

	// Parse Spotify URL to determine type
	urlType, spotifyID := parseSpotifyURL(req.URL)
	if urlType == "" {
		s.jsonResponse(w, http.StatusBadRequest, ImportResponse{
			Success: false,
			Error:   "Invalid Spotify URL. Supported: track, album, playlist",
		})
		return
	}

	// Create job
	jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
	job := &ImportJob{
		ID:        jobID,
		URL:       req.URL,
		Type:      urlType,
		Status:    "queued",
		CreatedAt: time.Now(),
	}

	// Store job
	s.jobsMutex.Lock()
	s.jobs[jobID] = job
	s.jobsMutex.Unlock()

	// Queue job for processing
	go s.processImportJob(job, req, spotifyID)

	s.jsonResponse(w, http.StatusAccepted, ImportResponse{
		Success: true,
		JobID:   jobID,
		Message: fmt.Sprintf("%s queued for import", urlType),
	})
}

// queueHandler handles GET/DELETE /api/queue
func (s *Server) queueHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		queueInfo := backend.GetDownloadQueue()
		resp := QueueResponse{
			Items:     queueInfo.Queue,
			Total:     len(queueInfo.Queue),
			Completed: queueInfo.CompletedCount,
			Failed:    queueInfo.FailedCount,
			Queued:    queueInfo.QueuedCount,
			Skipped:   queueInfo.SkippedCount,
		}
		s.jsonResponse(w, http.StatusOK, resp)

	case http.MethodDelete:
		backend.ClearAllDownloads()
		s.jsonResponse(w, http.StatusOK, map[string]string{"message": "Queue cleared"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// progressHandler handles GET /api/progress
func (s *Server) progressHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	progress := backend.GetDownloadProgress()
	resp := ProgressResponse{
		IsDownloading: progress.IsDownloading,
		CurrentSpeed:  progress.SpeedMBps,
		MBDownloaded:  progress.MBDownloaded,
	}
	s.jsonResponse(w, http.StatusOK, resp)
}

// historyHandler handles GET /api/history
func (s *Server) historyHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		history, err := backend.GetHistoryItems("SpotiFLAC")
		if err != nil {
			s.jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.jsonResponse(w, http.StatusOK, history)

	case http.MethodDelete:
		if err := backend.ClearHistory("SpotiFLAC"); err != nil {
			s.jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.jsonResponse(w, http.StatusOK, map[string]string{"message": "History cleared"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// jobsHandler handles GET /api/jobs
func (s *Server) jobsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.jobsMutex.RLock()
	jobs := make([]*ImportJob, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	s.jobsMutex.RUnlock()

	s.jsonResponse(w, http.StatusOK, jobs)
}

// processJobs processes jobs from the queue (placeholder for future async processing)
func (s *Server) processJobs() {
	// This goroutine can be expanded for more complex async job processing
	for job := range s.jobQueue {
		log.Printf("Processing job: %s", job.ID)
	}
}

// processImportJob processes an import job
func (s *Server) processImportJob(job *ImportJob, req ImportRequest, spotifyID string) {
	s.updateJobStatus(job.ID, "processing", "")

	// Determine service and quality
	service := s.config.Service
	if req.Service != "" {
		service = req.Service
	}

	quality := s.config.Quality
	if req.Quality != "" {
		quality = req.Quality
	}

	embedLyrics := s.config.EmbedLyrics
	if req.EmbedLyrics != nil {
		embedLyrics = *req.EmbedLyrics
	}

	embedCover := s.config.EmbedCover
	if req.EmbedCover != nil {
		embedCover = *req.EmbedCover
	}

	// Fetch metadata from Spotify
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	log.Printf("Fetching metadata for %s: %s", job.Type, req.URL)

	metadataJSON, err := backend.GetFilteredSpotifyData(ctx, req.URL, true, time.Second, ", ", nil)
	if err != nil {
		s.updateJobStatus(job.ID, "failed", fmt.Sprintf("Failed to fetch metadata: %v", err))
		return
	}

	// Parse metadata response - convert to JSON and back to get map structure
	jsonBytes, err := json.Marshal(metadataJSON)
	if err != nil {
		s.updateJobStatus(job.ID, "failed", fmt.Sprintf("Failed to marshal metadata: %v", err))
		return
	}

	// Log first 500 chars of metadata for debugging
	logLen := len(jsonBytes)
	if logLen > 500 {
		logLen = 500
	}
	log.Printf("Raw metadata JSON (first %d chars): %s", logLen, string(jsonBytes[:logLen]))

	var metadata map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &metadata); err != nil {
		s.updateJobStatus(job.ID, "failed", fmt.Sprintf("Failed to parse metadata: %v", err))
		return
	}

	// Extract tracks based on type
	// Structure from backend:
	// - Album: {"album_info": {...}, "track_list": [...]}
	// - Playlist: {"playlist_info": {...}, "track_list": [...]}
	// - Track: direct track object with fields like "spotify_id", "name", etc.
	var tracks []map[string]interface{}

	switch job.Type {
	case "track":
		// For single track, the metadata IS the track
		if name, ok := metadata["name"].(string); ok {
			job.Name = name
			tracks = append(tracks, metadata)
		}
	case "album":
		// Get album name from album_info
		if albumInfo, ok := metadata["album_info"].(map[string]interface{}); ok {
			if name, ok := albumInfo["name"].(string); ok {
				job.Name = name
			}
		}
		// Get tracks from track_list
		if trackList, ok := metadata["track_list"].([]interface{}); ok {
			for _, t := range trackList {
				if track, ok := t.(map[string]interface{}); ok {
					tracks = append(tracks, track)
				}
			}
		}
	case "playlist":
		// Get playlist name from playlist_info
		if playlistInfo, ok := metadata["playlist_info"].(map[string]interface{}); ok {
			if owner, ok := playlistInfo["owner"].(map[string]interface{}); ok {
				if name, ok := owner["name"].(string); ok {
					job.Name = name
				}
			}
		}
		// Get tracks from track_list
		if trackList, ok := metadata["track_list"].([]interface{}); ok {
			for _, t := range trackList {
				if track, ok := t.(map[string]interface{}); ok {
					tracks = append(tracks, track)
				}
			}
		}

	case "artist":
		// For artist, we get album_list and need to fetch each album's tracks
		// Structure: {"artist_info": {...}, "album_list": [...], "track_list": [...]}
		artistName := ""
		if artistInfo, ok := metadata["artist_info"].(map[string]interface{}); ok {
			if name, ok := artistInfo["name"].(string); ok {
				job.Name = name
				artistName = name
			}
		}

		// Get album list and filter only "album" type (not singles, EPs, compilations)
		var albumsToDownload []map[string]interface{}
		if albumList, ok := metadata["album_list"].([]interface{}); ok {
			for _, a := range albumList {
				if album, ok := a.(map[string]interface{}); ok {
					albumType := strings.ToLower(getStringField(album, "album_type"))
					albumName := getStringField(album, "name")
					// Only include main albums (not singles, eps, compilations)
					// Also exclude deluxe editions, live albums, re-issues
					isMainAlbum := albumType == "album"
					isDeluxe := strings.Contains(strings.ToLower(albumName), "deluxe")
					isLive := strings.Contains(strings.ToLower(albumName), "live") || strings.Contains(strings.ToLower(albumName), "l+1ve") || strings.Contains(strings.ToLower(albumName), "l-1ve")
					isReissue := strings.Contains(strings.ToLower(albumName), "re-issue") || strings.Contains(strings.ToLower(albumName), "reissue")

					if isMainAlbum && !isDeluxe && !isLive && !isReissue {
						albumsToDownload = append(albumsToDownload, album)
						log.Printf("Including album: %s (type: %s)", albumName, albumType)
					} else {
						log.Printf("Skipping: %s (type: %s, deluxe: %v, live: %v, reissue: %v)", albumName, albumType, isDeluxe, isLive, isReissue)
					}
				}
			}
		}

		log.Printf("Found %d albums to download for artist %s", len(albumsToDownload), artistName)

		// For each album, fetch its tracks
		for _, album := range albumsToDownload {
			albumID := getStringField(album, "id")
			albumName := getStringField(album, "name")
			if albumID == "" {
				continue
			}

			albumURL := fmt.Sprintf("https://open.spotify.com/album/%s", albumID)
			log.Printf("Fetching tracks for album: %s", albumName)

			// Fetch album metadata
			albumMetadataJSON, err := backend.GetFilteredSpotifyData(ctx, albumURL, true, time.Second, ", ", nil)
			if err != nil {
				log.Printf("Failed to fetch album %s: %v", albumName, err)
				continue
			}

			// Parse album metadata
			albumJsonBytes, err := json.Marshal(albumMetadataJSON)
			if err != nil {
				continue
			}

			var albumMetadata map[string]interface{}
			if err := json.Unmarshal(albumJsonBytes, &albumMetadata); err != nil {
				continue
			}

			// Get tracks from album
			if trackList, ok := albumMetadata["track_list"].([]interface{}); ok {
				for _, t := range trackList {
					if track, ok := t.(map[string]interface{}); ok {
						tracks = append(tracks, track)
					}
				}
			}
		}
	}

	job.TracksTotal = len(tracks)
	log.Printf("Found %d tracks for job %s (%s)", len(tracks), job.ID, job.Name)

	if len(tracks) == 0 {
		s.updateJobStatus(job.ID, "failed", "No tracks found")
		return
	}

	// Build output directory structure for Lidarr compatibility
	// Format: Artist Name - Album Name (Year)/
	outputDir := s.config.OutputDir

	// Process each track
	successCount := 0
	for i, track := range tracks {
		// Field names from AlbumTrackMetadata struct:
		// spotify_id, name, artists, album_name, album_artist, images, release_date,
		// track_number, disc_number, total_tracks, total_discs, duration_ms
		trackName := getStringField(track, "name")
		artistName := getStringField(track, "artists")
		albumName := getStringField(track, "album_name")
		albumArtist := getStringField(track, "album_artist")
		releaseDate := getStringField(track, "release_date")
		coverURL := getStringField(track, "images")
		trackSpotifyID := getStringField(track, "spotify_id")
		trackNumber := getIntField(track, "track_number")
		discNumber := getIntField(track, "disc_number")
		totalTracks := getIntField(track, "total_tracks")
		totalDiscs := getIntField(track, "total_discs")
		durationMS := getIntField(track, "duration_ms")
		duration := durationMS / 1000 // Convert to seconds
		copyright := getStringField(track, "copyright")
		publisher := getStringField(track, "publisher")

		// Build track-specific output directory
		// Structure: {Artist Name}/{Album Title}/
		trackOutputDir := outputDir

		// Use album artist if available, otherwise track artist
		folderArtist := albumArtist
		if folderArtist == "" {
			folderArtist = artistName
		}

		if folderArtist != "" && albumName != "" {
			// Create folder structure: Artist Name/Album Title/
			artistFolder := backend.SanitizeFilename(folderArtist)
			albumFolder := backend.SanitizeFilename(albumName)
			trackOutputDir = filepath.Join(outputDir, artistFolder, albumFolder)
		} else if folderArtist != "" {
			// Single track without album - just use artist folder
			artistFolder := backend.SanitizeFilename(folderArtist)
			trackOutputDir = filepath.Join(outputDir, artistFolder)
		}

		// Ensure output directory exists
		if err := os.MkdirAll(trackOutputDir, 0755); err != nil {
			log.Printf("Error creating output directory: %v", err)
			continue
		}

		log.Printf("Downloading track %d/%d: %s - %s", i+1, len(tracks), artistName, trackName)

		// Use track number from metadata if available, otherwise use position
		trackNum := trackNumber
		if trackNum == 0 {
			trackNum = i + 1
		}

		// Create download request
		// Filename format: {year} - {track} - {title}
		// This produces: 2023 - 01 - Track Title.flac
		downloadReq := DownloadRequest{
			Service:              service,
			TrackName:            trackName,
			ArtistName:           artistName,
			AlbumName:            albumName,
			AlbumArtist:          albumArtist,
			ReleaseDate:          releaseDate,
			CoverURL:             coverURL,
			OutputDir:            trackOutputDir,
			AudioFormat:          quality,
			FilenameFormat:       "{year} - {track} - {title}",
			TrackNumber:          true,
			Position:             trackNum,
			UseAlbumTrackNumber:  true,
			SpotifyID:            trackSpotifyID,
			EmbedLyrics:          embedLyrics,
			EmbedMaxQualityCover: embedCover,
			Duration:             duration,
			SpotifyTrackNumber:   trackNumber,
			SpotifyDiscNumber:    discNumber,
			SpotifyTotalTracks:   totalTracks,
			SpotifyTotalDiscs:    totalDiscs,
			Copyright:            copyright,
			Publisher:            publisher,
			AllowFallback:        true,
		}

		// Download the track
		resp, err := s.downloadTrack(downloadReq)
		if err != nil {
			log.Printf("Failed to download track %s: %v", trackName, err)
			continue
		}

		if resp.Success {
			successCount++
			job.TracksQueued = successCount
			log.Printf("Successfully downloaded: %s", resp.File)
		} else {
			log.Printf("Download failed for %s: %s", trackName, resp.Error)
		}
	}

	if successCount == len(tracks) {
		s.updateJobStatus(job.ID, "completed", "")
	} else if successCount > 0 {
		s.updateJobStatus(job.ID, "completed", fmt.Sprintf("Partial: %d/%d tracks downloaded", successCount, len(tracks)))
	} else {
		s.updateJobStatus(job.ID, "failed", "All tracks failed to download")
	}

	log.Printf("Job %s completed: %d/%d tracks downloaded", job.ID, successCount, len(tracks))
}

// downloadTrack downloads a single track using the existing backend logic
func (s *Server) downloadTrack(req DownloadRequest) (DownloadResponse, error) {
	// This reuses the download logic from app.go but adapted for server mode
	if req.Service == "" {
		req.Service = s.config.Service
	}

	if req.OutputDir == "" {
		req.OutputDir = s.config.OutputDir
	}

	if req.AudioFormat == "" {
		req.AudioFormat = s.config.Quality
	}

	if req.FilenameFormat == "" {
		req.FilenameFormat = s.config.FilenameFormat
	}

	var filename string
	var err error

	itemID := fmt.Sprintf("%s-%d", req.SpotifyID, time.Now().UnixNano())
	backend.AddToQueue(itemID, req.TrackName, req.ArtistName, req.AlbumName, req.SpotifyID)
	backend.SetDownloading(true)
	backend.StartDownloadItem(itemID)
	defer backend.SetDownloading(false)

	spotifyURL := ""
	if req.SpotifyID != "" {
		spotifyURL = fmt.Sprintf("https://open.spotify.com/track/%s", req.SpotifyID)
	}

	// Fetch lyrics in background if enabled
	lyricsChan := make(chan string, 1)
	if req.SpotifyID != "" && req.EmbedLyrics {
		go func() {
			client := backend.NewLyricsClient()
			resp, _, err := client.FetchLyricsAllSources(req.SpotifyID, req.TrackName, req.ArtistName, req.AlbumName, req.Duration)
			if err == nil && resp != nil && len(resp.Lines) > 0 {
				lrc := client.ConvertToLRC(resp, req.TrackName, req.ArtistName)
				lyricsChan <- lrc
			} else {
				lyricsChan <- ""
			}
		}()
	} else {
		close(lyricsChan)
	}

	// Download based on service
	switch req.Service {
	case "amazon":
		downloader := backend.NewAmazonDownloader()
		filename, err = downloader.DownloadBySpotifyID(
			req.SpotifyID, req.OutputDir, req.AudioFormat, req.FilenameFormat,
			"", "", req.TrackNumber, req.Position,
			req.TrackName, req.ArtistName, req.AlbumName, req.AlbumArtist, req.ReleaseDate,
			req.CoverURL, req.SpotifyTrackNumber, req.SpotifyDiscNumber, req.SpotifyTotalTracks,
			req.EmbedMaxQualityCover, req.SpotifyTotalDiscs, req.Copyright, req.Publisher,
			spotifyURL, false, false, false,
		)

	case "tidal":
		downloader := backend.NewTidalDownloader("")
		filename, err = downloader.Download(
			req.SpotifyID, req.OutputDir, req.AudioFormat, req.FilenameFormat,
			req.TrackNumber, req.Position,
			req.TrackName, req.ArtistName, req.AlbumName, req.AlbumArtist, req.ReleaseDate,
			req.UseAlbumTrackNumber, req.CoverURL, req.EmbedMaxQualityCover,
			req.SpotifyTrackNumber, req.SpotifyDiscNumber, req.SpotifyTotalTracks, req.SpotifyTotalDiscs,
			req.Copyright, req.Publisher, spotifyURL, req.AllowFallback, false, false, false,
		)

	case "qobuz":
		// Get ISRC for Qobuz
		client := backend.NewSongLinkClient()
		isrc, _ := client.GetISRCDirect(req.SpotifyID)

		downloader := backend.NewQobuzDownloader()
		quality := req.AudioFormat
		if quality == "" || quality == "LOSSLESS" {
			quality = "6" // FLAC 16-bit
		}
		filename, err = downloader.DownloadTrackWithISRC(
			isrc, req.SpotifyID, req.OutputDir, quality, req.FilenameFormat,
			req.TrackNumber, req.Position,
			req.TrackName, req.ArtistName, req.AlbumName, req.AlbumArtist, req.ReleaseDate,
			req.UseAlbumTrackNumber, req.CoverURL, req.EmbedMaxQualityCover,
			req.SpotifyTrackNumber, req.SpotifyDiscNumber, req.SpotifyTotalTracks, req.SpotifyTotalDiscs,
			req.Copyright, req.Publisher, spotifyURL, req.AllowFallback, false, false, false,
		)

	default:
		return DownloadResponse{
			Success: false,
			Error:   fmt.Sprintf("Unknown service: %s", req.Service),
		}, fmt.Errorf("unknown service: %s", req.Service)
	}

	if err != nil {
		backend.FailDownloadItem(itemID, fmt.Sprintf("Download failed: %v", err))
		return DownloadResponse{
			Success: false,
			Error:   fmt.Sprintf("Download failed: %v", err),
			ItemID:  itemID,
		}, err
	}

	// Check if file already exists
	alreadyExists := false
	if strings.HasPrefix(filename, "EXISTS:") {
		alreadyExists = true
		filename = strings.TrimPrefix(filename, "EXISTS:")
	}

	// Embed lyrics if downloaded successfully
	if !alreadyExists && req.SpotifyID != "" && req.EmbedLyrics {
		select {
		case lyrics := <-lyricsChan:
			if lyrics != "" && (strings.HasSuffix(filename, ".flac") || strings.HasSuffix(filename, ".mp3")) {
				if err := backend.EmbedLyricsOnlyUniversal(filename, lyrics); err != nil {
					log.Printf("Failed to embed lyrics: %v", err)
				} else {
					log.Printf("Lyrics embedded successfully")
				}
			}
		case <-time.After(30 * time.Second):
			log.Printf("Lyrics fetch timed out")
		}
	}

	message := "Download completed successfully"
	if alreadyExists {
		message = "File already exists"
		backend.SkipDownloadItem(itemID, filename)
	} else {
		if fileInfo, statErr := os.Stat(filename); statErr == nil {
			finalSize := float64(fileInfo.Size()) / (1024 * 1024)
			backend.CompleteDownloadItem(itemID, filename, finalSize)
		} else {
			backend.CompleteDownloadItem(itemID, filename, 0)
		}
	}

	return DownloadResponse{
		Success:       true,
		Message:       message,
		File:          filename,
		AlreadyExists: alreadyExists,
		ItemID:        itemID,
	}, nil
}

// updateJobStatus updates a job's status
func (s *Server) updateJobStatus(jobID, status, errorMsg string) {
	s.jobsMutex.Lock()
	defer s.jobsMutex.Unlock()

	if job, ok := s.jobs[jobID]; ok {
		job.Status = status
		if errorMsg != "" {
			job.Error = errorMsg
		}
	}
}

// jsonResponse sends a JSON response
func (s *Server) jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// parseSpotifyURL parses a Spotify URL and returns the type and ID
func parseSpotifyURL(url string) (string, string) {
	// Patterns for Spotify URLs
	patterns := map[string]*regexp.Regexp{
		"track":    regexp.MustCompile(`spotify\.com/track/([a-zA-Z0-9]+)`),
		"album":    regexp.MustCompile(`spotify\.com/album/([a-zA-Z0-9]+)`),
		"playlist": regexp.MustCompile(`spotify\.com/playlist/([a-zA-Z0-9]+)`),
		"artist":   regexp.MustCompile(`spotify\.com/artist/([a-zA-Z0-9]+)`),
	}

	for urlType, pattern := range patterns {
		if matches := pattern.FindStringSubmatch(url); len(matches) > 1 {
			return urlType, matches[1]
		}
	}

	return "", ""
}

// Helper functions to extract fields from metadata
func getStringField(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getIntField(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok {
		switch val := v.(type) {
		case int:
			return val
		case float64:
			return int(val)
		case int64:
			return int(val)
		}
	}
	return 0
}
