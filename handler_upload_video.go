package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const uploadLimit = 1 << 30 // 1 GB
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)

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

	fmt.Println("uploading video", videoID, "by user", userID)

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
		return
	}
	if userID != video.UserID {
		respondWithError(w, http.StatusUnauthorized, "Current user can't modify this video", errors.New("Incorrect auth"))
		return
	}

	err = r.ParseMultipartForm(uploadLimit)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not parse form contents", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not get media type", err)
		return
	}

	if contentType == "" {
		respondWithError(w, http.StatusBadRequest, "Missing content type header", nil)
		return
	}

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid media type", nil)
		return
	}

	tmpFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not get media type", err)
		return
	}

	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	defer tmpFile.Close()

	_, err = io.Copy(tmpFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create tmp file correctly", err)
		return
	}

	_, err = tmpFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not point to beginning of file", err)
		return
	}

	proccessedVideoPath, err := processVideoForFastStart(tmpPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not process video", err)
		return
	}

	processedVideo, err := os.Open(proccessedVideoPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not open processed video", err)
		return
	}

	randID := make([]byte, 32)
	_, err = rand.Read(randID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create resource ID", err)
		return
	}

	resourceID := base64.RawURLEncoding.EncodeToString((randID))
	aspectRatio, err := getVideoAspectRatio(proccessedVideoPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not get aspect ratio", err)
		return
	}

	fullResourceID := aspectRatio + "/" + resourceID // e.g. portrait/vertical.mp4

	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         aws.String(fullResourceID),
		Body:        processedVideo,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "upload error", err)
		return
	}

	bucketKey := fmt.Sprintf("%s,%s", cfg.s3Bucket, fullResourceID) // e.g. tubely,portrait/vertical.mp4
	fmt.Printf("%s", bucketKey)
	video.VideoURL = &bucketKey

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not update video", err)
		return
	}

	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not convert video URL to signed URL", err)
		return
	}
	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	type Result struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	buf := new(bytes.Buffer)
	cmd.Stdout = buf
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		log.Printf("Command finished with error: %v", err)
		return "", err
	}

	var result Result

	err = json.Unmarshal(buf.Bytes(), &result)
	if err != nil {
		return "", err
	}

	width := float64(result.Streams[0].Width)
	height := float64(result.Streams[0].Height)
	aspectRatio := width / height

	if len(result.Streams) == 0 {
		return "", fmt.Errorf("no streams found in the video")
	}

	// 16:9 ≈ 1.777777...
	if aspectRatio > 1.7 && aspectRatio < 1.8 {
		return "landscape", nil
	}

	// 9:16 ≈ 0.5625
	if aspectRatio > 0.55 && aspectRatio < 0.57 {
		return "portrait", nil
	}

	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	newFilePath := filePath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", newFilePath)
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return "", err
	}

	fileInfo, err := os.Stat(newFilePath)
	if err != nil {
		return "", fmt.Errorf("could not start processed file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return newFilePath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)

	req := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}

	presignedReq, err := presignClient.PresignGetObject(context.Background(), req, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to presign request: %w", err)
	}

	return presignedReq.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil // here I had a problem, I was returning an emtpy video. It was resetting the video to zero values
	}

	bucketKey := strings.Split(*video.VideoURL, ",")

	fmt.Println(bucketKey)

	if len(bucketKey) < 2 {
		return video, fmt.Errorf("could not split video URL") // the same problem as above, I was returning an empty video
	}

	bucket := bucketKey[0]
	key := bucketKey[1]

	presignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, time.Minute*15)
	if err != nil {
		return video, fmt.Errorf("failed to generate presigned URL: %w", err) // god, no wonder it didn't work
	}

	video.VideoURL = &presignedURL

	return video, nil
}
