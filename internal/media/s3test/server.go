// Package s3test is a fake S3 endpoint for tests: the calls core's R2
// adapter makes (objects, ranged reads, copies with new metadata) and the
// multipart upload a Direct upload is (create, upload a part, list parts,
// complete, abort, list the open uploads), with a part copied from another
// object's byte range (UploadPartCopy). It keeps everything in memory,
// speaks path-style addresses (/<bucket>/<key>) and checks no signature: a
// presigned part address works when the test PUTs to it as a browser would.
// It joins parts only as R2 does: every part but the last the same size,
// and at least 5 MiB.
package s3test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Object is a stored object and the metadata it was stored with.
type Object struct {
	Data               []byte
	ContentType        string
	ContentDisposition string
	ETag               string
}

// minPartSize is the smallest part R2 joins, but for the last.
const minPartSize = 5 << 20

type part struct {
	data []byte
	etag string
}

type upload struct {
	bucket, key string
	parts       map[int32]part
	// meta is what the object is stored with once completed: the
	// metadata the upload was created with.
	meta Object
}

// Server is the fake.
type Server struct {
	*httptest.Server

	mu      sync.Mutex
	objects map[string]Object // bucket + "/" + key
	uploads map[string]*upload
	lastID  int
	aborted []string
	// failures answers the next requests of an operation with an S3 error.
	failures map[string][]failure
	// partQueries are the query strings of the part uploads received: the
	// presigned addresses' signing parameters.
	partQueries []url.Values
	// counts are the requests received, by operation.
	counts map[string]int
	// served counts the object bytes GetObject answered.
	served int64
	// holds keep an operation's requests waiting (Hold).
	holds map[string]*hold
	t     testing.TB
}

type hold struct {
	entered  chan struct{}
	released chan struct{}
}

type failure struct {
	status int
	code   string
}

// New starts the fake; it stops with the test.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		objects: map[string]Object{}, uploads: map[string]*upload{}, failures: map[string][]failure{},
		counts: map[string]int{}, holds: map[string]*hold{}, t: t,
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Fail makes the next request of the operation (CreateMultipartUpload,
// UploadPart, UploadPartCopy, ListParts, CompleteMultipartUpload, AbortMultipartUpload,
// ListMultipartUploads, PutObject, CopyObject, GetObject, HeadObject,
// DeleteObject) answer with the status and S3 error code.
func (s *Server) Fail(operation string, status int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[operation] = append(s.failures[operation], failure{status: status, code: code})
}

// Object returns the object stored at key in bucket.
func (s *Server) Object(bucket, key string) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[bucket+"/"+key]
	return o, ok
}

// Keys are the keys stored in bucket, sorted.
func (s *Server) Keys(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for name := range s.objects {
		if b, key, _ := strings.Cut(name, "/"); b == bucket {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// OpenUploads are the keys of the multipart uploads open in bucket, sorted.
func (s *Server) OpenUploads(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for _, u := range s.uploads {
		if u.bucket == bucket {
			keys = append(keys, u.key)
		}
	}
	slices.Sort(keys)
	return keys
}

// UploadMetadata is the metadata of the multipart upload open at key in
// bucket, as it was created: what the object is stored with once it is
// completed.
func (s *Server) UploadMetadata(bucket, key string) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.uploads {
		if u.bucket == bucket && u.key == key {
			return u.meta, true
		}
	}
	return Object{}, false
}

// Hold keeps every request of the operation waiting, as a slow storage
// would, until release is called (the test's end calls it too). entered
// receives once for each request that starts waiting.
func (s *Server) Hold(operation string) (entered <-chan struct{}, release func()) {
	h := &hold{entered: make(chan struct{}, 64), released: make(chan struct{})}
	s.mu.Lock()
	s.holds[operation] = h
	s.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			s.mu.Lock()
			if s.holds[operation] == h {
				delete(s.holds, operation)
			}
			s.mu.Unlock()
			close(h.released)
		})
	}
	s.t.Cleanup(release)
	return h.entered, release
}

// Count is how many requests of the operation the fake received.
func (s *Server) Count(operation string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[operation]
}

// Served is how many object bytes GetObject answered, ranged reads
// included: what core downloaded.
func (s *Server) Served() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served
}

// Aborted are the keys whose multipart upload was aborted, in order.
func (s *Server) Aborted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.aborted)
}

// PartQueries are the query strings of the part uploads received, in order.
func (s *Server) PartQueries() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.partQueries)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	operation := operationOf(r, key, q)
	s.mu.Lock()
	s.counts[operation]++
	held := s.holds[operation]
	s.mu.Unlock()
	if held != nil {
		held.entered <- struct{}{}
		<-held.released
	}
	if f, failed := s.takeFailure(operation); failed {
		writeError(w, f.status, f.code)
		return
	}
	switch operation {
	case "CreateMultipartUpload":
		s.createUpload(w, r, bucket, key)
	case "UploadPart":
		s.uploadPart(w, r, q)
	case "UploadPartCopy":
		s.uploadPartCopy(w, r, bucket, key, q)
	case "ListParts":
		s.listParts(w, bucket, key, q.Get("uploadId"))
	case "CompleteMultipartUpload":
		s.complete(w, r, bucket, key, q.Get("uploadId"))
	case "AbortMultipartUpload":
		s.abort(w, q.Get("uploadId"))
	case "ListMultipartUploads":
		s.listUploads(w, bucket, q.Get("prefix"))
	case "CopyObject":
		s.copyObject(w, r, bucket, key)
	case "PutObject":
		s.putObject(w, r, bucket, key)
	case "HeadObject", "GetObject":
		s.getObject(w, r, bucket, key, operation == "HeadObject")
	case "DeleteObject":
		s.mu.Lock()
		delete(s.objects, bucket+"/"+key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusNotImplemented, "NotImplemented")
	}
}

func operationOf(r *http.Request, key string, q url.Values) string {
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		return "CreateMultipartUpload"
	case r.Method == http.MethodPut && q.Has("uploadId") && q.Has("partNumber") && r.Header.Get("X-Amz-Copy-Source") != "":
		return "UploadPartCopy"
	case r.Method == http.MethodPut && q.Has("uploadId") && q.Has("partNumber"):
		return "UploadPart"
	case r.Method == http.MethodGet && q.Has("uploadId"):
		return "ListParts"
	case r.Method == http.MethodPost && q.Has("uploadId"):
		return "CompleteMultipartUpload"
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		return "AbortMultipartUpload"
	case r.Method == http.MethodGet && key == "" && q.Has("uploads"):
		return "ListMultipartUploads"
	case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
		return "CopyObject"
	case r.Method == http.MethodPut:
		return "PutObject"
	case r.Method == http.MethodHead:
		return "HeadObject"
	case r.Method == http.MethodGet:
		return "GetObject"
	case r.Method == http.MethodDelete:
		return "DeleteObject"
	}
	return ""
}

func (s *Server) takeFailure(operation string) (failure, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queued := s.failures[operation]
	if len(queued) == 0 {
		return failure{}, false
	}
	s.failures[operation] = queued[1:]
	return queued[0], true
}

func (s *Server) createUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	s.mu.Lock()
	s.lastID++
	id := "upload-" + strconv.Itoa(s.lastID)
	s.uploads[id] = &upload{bucket: bucket, key: key, parts: map[int32]part{}, meta: Object{
		ContentType: r.Header.Get("Content-Type"), ContentDisposition: r.Header.Get("Content-Disposition"),
	}}
	s.mu.Unlock()
	writeXML(w, http.StatusOK, initiateResult{Bucket: bucket, Key: key, UploadID: id})
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, q url.Values) {
	number, err := strconv.ParseInt(q.Get("partNumber"), 10, 32)
	if err != nil || number < 1 || number > 10000 {
		writeError(w, http.StatusBadRequest, "InvalidArgument")
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "IncompleteBody")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[q.Get("uploadId")]
	if !ok {
		writeError(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	s.partQueries = append(s.partQueries, q)
	etag := etagOf(data)
	u.parts[int32(number)] = part{data: data, etag: etag}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// uploadPartCopy stores as a part the byte range of another object that
// X-Amz-Copy-Source-Range names (all of it without one).
func (s *Server) uploadPartCopy(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values) {
	number, err := strconv.ParseInt(q.Get("partNumber"), 10, 32)
	if err != nil || number < 1 || number > 10000 {
		writeError(w, http.StatusBadRequest, "InvalidArgument")
		return
	}
	source, err := url.PathUnescape(strings.TrimPrefix(r.Header.Get("X-Amz-Copy-Source"), "/"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[q.Get("uploadId")]
	if !ok || u.bucket != bucket || u.key != key {
		writeError(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	src, ok := s.objects[source]
	if !ok {
		writeError(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	data := src.Data
	if spec := r.Header.Get("X-Amz-Copy-Source-Range"); spec != "" {
		first, last, ok := exactRange(spec, int64(len(src.Data)))
		if !ok {
			writeError(w, http.StatusBadRequest, "InvalidArgument")
			return
		}
		data = src.Data[first : last+1]
	}
	etag := etagOf(data)
	u.parts[int32(number)] = part{data: slices.Clone(data), etag: etag}
	writeXML(w, http.StatusOK, copyPartResult{ETag: etag, LastModified: timestamp()})
}

// exactRange reads "bytes=first-last", which must lie within the object:
// a part copy is never clamped.
func exactRange(spec string, size int64) (int64, int64, bool) {
	a, b, ok := strings.Cut(strings.TrimPrefix(spec, "bytes="), "-")
	first, errA := strconv.ParseInt(a, 10, 64)
	last, errB := strconv.ParseInt(b, 10, 64)
	if !ok || !strings.HasPrefix(spec, "bytes=") || errA != nil || errB != nil || first < 0 || first > last || last >= size {
		return 0, 0, false
	}
	return first, last, true
}

func (s *Server) listParts(w http.ResponseWriter, bucket, key, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[id]
	if !ok || u.bucket != bucket || u.key != key {
		writeError(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	result := listPartsResult{Bucket: bucket, Key: key, UploadID: id}
	for _, number := range slices.Sorted(maps.Keys(u.parts)) {
		p := u.parts[number]
		result.Parts = append(result.Parts, listedPart{PartNumber: number, ETag: p.etag, Size: int64(len(p.data)), LastModified: timestamp()})
	}
	writeXML(w, http.StatusOK, result)
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request, bucket, key, id string) {
	var body completeRequest
	if err := xml.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[id]
	if !ok || u.bucket != bucket || u.key != key {
		writeError(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	if len(body.Parts) == 0 {
		writeError(w, http.StatusBadRequest, "MalformedXML")
		return
	}
	var data bytes.Buffer
	var sums []byte
	for i, p := range body.Parts {
		if i > 0 && p.PartNumber <= body.Parts[i-1].PartNumber {
			writeError(w, http.StatusBadRequest, "InvalidPartOrder")
			return
		}
		stored, ok := u.parts[p.PartNumber]
		if !ok || stored.etag != p.ETag {
			writeError(w, http.StatusBadRequest, "InvalidPart")
			return
		}
		if i < len(body.Parts)-1 {
			// R2: every part but the last at least 5 MiB, and all of them
			// the same size (the last no larger).
			if len(stored.data) < minPartSize {
				writeError(w, http.StatusBadRequest, "EntityTooSmall")
				return
			}
			if first := u.parts[body.Parts[0].PartNumber]; len(stored.data) != len(first.data) {
				writeError(w, http.StatusBadRequest, "InvalidPart")
				return
			}
		} else if i > 0 && len(stored.data) > len(u.parts[body.Parts[0].PartNumber].data) {
			writeError(w, http.StatusBadRequest, "InvalidPart")
			return
		}
		data.Write(stored.data)
		sum, _ := hex.DecodeString(strings.Trim(stored.etag, `"`))
		sums = append(sums, sum...)
	}
	whole := md5.Sum(sums)
	etag := fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(whole[:]), len(body.Parts))
	s.objects[bucket+"/"+key] = Object{Data: data.Bytes(), ContentType: u.meta.ContentType, ContentDisposition: u.meta.ContentDisposition, ETag: etag}
	delete(s.uploads, id)
	writeXML(w, http.StatusOK, completeResult{Bucket: bucket, Key: key, ETag: etag})
}

func (s *Server) abort(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[id]
	if !ok {
		writeError(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	s.aborted = append(s.aborted, u.key)
	delete(s.uploads, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listUploads(w http.ResponseWriter, bucket, prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := listUploadsResult{Bucket: bucket, Prefix: prefix}
	for _, id := range slices.Sorted(maps.Keys(s.uploads)) {
		u := s.uploads[id]
		if u.bucket == bucket && strings.HasPrefix(u.key, prefix) {
			result.Uploads = append(result.Uploads, listedUpload{Key: u.key, UploadID: id, Initiated: timestamp()})
		}
	}
	writeXML(w, http.StatusOK, result)
}

func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	source, err := url.PathUnescape(strings.TrimPrefix(r.Header.Get("X-Amz-Copy-Source"), "/"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.objects[source]
	if !ok {
		writeError(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	copied := Object{Data: slices.Clone(src.Data), ContentType: src.ContentType, ContentDisposition: src.ContentDisposition, ETag: src.ETag}
	if strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE") {
		copied.ContentType = r.Header.Get("Content-Type")
		copied.ContentDisposition = r.Header.Get("Content-Disposition")
	}
	s.objects[bucket+"/"+key] = copied
	writeXML(w, http.StatusOK, copyResult{ETag: copied.ETag, LastModified: timestamp()})
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "IncompleteBody")
		return
	}
	etag := etagOf(data)
	s.mu.Lock()
	s.objects[bucket+"/"+key] = Object{
		Data: data, ContentType: r.Header.Get("Content-Type"), ContentDisposition: r.Header.Get("Content-Disposition"), ETag: etag,
	}
	s.mu.Unlock()
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string, head bool) {
	s.mu.Lock()
	o, ok := s.objects[bucket+"/"+key]
	s.mu.Unlock()
	if !ok {
		if head {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeError(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	w.Header().Set("ETag", o.ETag)
	if o.ContentType != "" {
		w.Header().Set("Content-Type", o.ContentType)
	}
	if o.ContentDisposition != "" {
		w.Header().Set("Content-Disposition", o.ContentDisposition)
	}
	data, status := o.Data, http.StatusOK
	if first, last, ranged := byteRange(r.Header.Get("Range"), int64(len(o.Data))); ranged {
		data, status = o.Data[first:last+1], http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(o.Data)))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if !head {
		s.mu.Lock()
		s.served += int64(len(data))
		s.mu.Unlock()
		_, _ = w.Write(data)
	}
}

// byteRange reads "bytes=first-last", clamped to the object's size.
func byteRange(header string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || size == 0 {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(spec, "-")
	first, err := strconv.ParseInt(a, 10, 64)
	if !ok || err != nil || first >= size {
		return 0, 0, false
	}
	last := size - 1
	if b != "" {
		if parsed, err := strconv.ParseInt(b, 10, 64); err == nil && parsed < last {
			last = parsed
		}
	}
	return first, last, true
}

func etagOf(data []byte) string {
	sum := md5.Sum(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func timestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

func writeXML(w http.ResponseWriter, status int, body any) {
	out, err := xml.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError")
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(out)
}

type initiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type listedPart struct {
	PartNumber   int32  `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

type listPartsResult struct {
	XMLName     xml.Name     `xml:"ListPartsResult"`
	Bucket      string       `xml:"Bucket"`
	Key         string       `xml:"Key"`
	UploadID    string       `xml:"UploadId"`
	IsTruncated bool         `xml:"IsTruncated"`
	Parts       []listedPart `xml:"Part"`
}

type completeRequest struct {
	Parts []struct {
		PartNumber int32  `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

type completeResult struct {
	XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
	Bucket  string   `xml:"Bucket"`
	Key     string   `xml:"Key"`
	ETag    string   `xml:"ETag"`
}

type listedUpload struct {
	Key       string `xml:"Key"`
	UploadID  string `xml:"UploadId"`
	Initiated string `xml:"Initiated"`
}

type listUploadsResult struct {
	XMLName     xml.Name       `xml:"ListMultipartUploadsResult"`
	Bucket      string         `xml:"Bucket"`
	Prefix      string         `xml:"Prefix"`
	IsTruncated bool           `xml:"IsTruncated"`
	Uploads     []listedUpload `xml:"Upload"`
}

type copyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

type copyResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}
