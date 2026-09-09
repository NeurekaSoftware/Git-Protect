package store

import (
	"path"
	"strings"
)

// DefaultContentType is the fallback for attachments without a known type.
const DefaultContentType = "application/octet-stream"

// deleteBatchSize is the S3 bulk-delete limit.
const deleteBatchSize = 1000

// mimeTypesByExtension maps the file kinds commonly attached to issues and
// merge requests to a best-effort content type.
var mimeTypesByExtension = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".svg":  "image/svg+xml",
	".ico":  "image/x-icon",
	".pdf":  "application/pdf",
	".txt":  "text/plain",
	".log":  "text/plain",
	".md":   "text/markdown",
	".csv":  "text/csv",
	".json": "application/json",
	".xml":  "application/xml",
	".yml":  "application/yaml",
	".yaml": "application/yaml",
	".zip":  "application/zip",
	".gz":   "application/gzip",
	".tar":  "application/x-tar",
	".7z":   "application/x-7z-compressed",
	".mp4":  "video/mp4",
	".mov":  "video/quicktime",
	".webm": "video/webm",
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// ResolveContentType maps a file name to a best-effort content type for stored
// attachments, falling back to DefaultContentType.
func ResolveContentType(fileName string) string {
	extension := strings.ToLower(path.Ext(fileName))
	if contentType, known := mimeTypesByExtension[extension]; known {
		return contentType
	}
	return DefaultContentType
}
