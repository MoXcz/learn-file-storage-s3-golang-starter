package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	const maxMemory = 10 << 20 // 10 MB
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not parse form contents", err)
		return
	}

	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not get media type", nil)
		return
	}

	if contentType == "" {
		respondWithError(w, http.StatusBadRequest, "Missing content type header", nil)
		return
	}

	if mediaType != "image/jpeg" && mediaType != "image/png" {
		respondWithError(w, http.StatusBadRequest, "Invalid media type", nil)
		return
	}

	_, fileExtension, _ := strings.Cut(contentType, "/")
	randID := make([]byte, 32)
	_, err = rand.Read(randID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create resource ID", nil)
		return
	}

	resourceID := base64.RawURLEncoding.EncodeToString((randID))

	dataURL := fmt.Sprintf("%s.%s", resourceID, fileExtension) // <videoID>.<file_extension>
	filepath := filepath.Join(cfg.assetsRoot, dataURL)
	createdFile, err := os.Create(filepath)
	defer createdFile.Close()
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create video file", nil)
		return
	}

	_, err = io.Copy(createdFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not save file", nil)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		return
	}
	if userID != video.UserID {
		respondWithError(w, http.StatusUnauthorized, "Current user can't modify this video", errors.New("Incorrect auth"))
		return
	}

	thumbnailURL := fmt.Sprintf("http://localhost:%s/%s", cfg.port, filepath)
	video.ThumbnailURL = &thumbnailURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not update thumbnail", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
