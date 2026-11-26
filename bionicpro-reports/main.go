package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/gin-gonic/gin"
)

const sessionCookieName = "session_id"
const frontendOrigin = "http://192.168.56.106:3000"

var httpClient = &http.Client{Timeout: 15 * time.Second}

type Config struct {
	Port             string
	DatabaseURL      string
	AuthURL          string
	InternalSecret   string
	OIDCIssuer       string
	OIDCAudience     string
	S3Endpoint       string
	S3Region         string
	S3AccessKey      string
	S3SecretKey      string
	S3Bucket         string
	CDNBaseURL       string
	CDNSigningSecret string
	CDNURLTTL        time.Duration
	ReportCacheTTL   time.Duration
}

type authSession struct {
	UserID      string    `json:"user_id"`
	Username    string    `json:"username"`
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"access_token_expires_at"`
}

type jwksResponse struct {
	Keys []jwk `json:"keys"`
}
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}
type jwtClaims struct {
	Issuer            string          `json:"iss"`
	Subject           string          `json:"sub"`
	PreferredUsername string          `json:"preferred_username"`
	Audience          json.RawMessage `json:"aud"`
	ExpiresAt         int64           `json:"exp"`
	NotBefore         int64           `json:"nbf"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
	ResourceAccess map[string]struct {
		Roles []string `json:"roles"`
	} `json:"resource_access"`
}

type reportRow struct {
	PatientID                              int64
	PatientIdentityID, FirstName, LastName string
	ProsthesisID                           int64
	ProsthesisModel, SerialNumber          string
	DateID                                 int
	MeasurementDate                        time.Time
	SensorID                               int64
	SensorType, Unit                       string
	MeasurementCount                       int64
	AvgValue, MinValue, MaxValue           float64
}

type reportManifest struct {
	ObjectKey   string    `json:"object_key"`
	GeneratedAt time.Time `json:"generated_at"`
	SHA256      string    `json:"sha256"`
}

type reportResponse struct {
	Date      string `json:"date"`
	URL       string `json:"url"`
	Format    string `json:"format"`
	Cached    bool   `json:"cached"`
	Generated string `json:"generated_at"`
}

type s3Store struct {
	endpoint  *url.URL
	region    string
	accessKey string
	secretKey string
	bucket    string
	client    *http.Client
}

func main() {
	cfg := Config{
		Port:             env("PORT", "8082"),
		DatabaseURL:      strings.TrimSpace(os.Getenv("OLAP_CLICKHOUSE_URL")),
		AuthURL:          strings.TrimRight(env("AUTH_INTERNAL_URL", "http://bionicpro-auth:8081"), "/"),
		InternalSecret:   os.Getenv("AUTH_INTERNAL_SECRET"),
		OIDCIssuer:       strings.TrimRight(env("OIDC_ISSUER", "http://192.168.56.106:8080/realms/reports-realm"), "/"),
		OIDCAudience:     strings.TrimSpace(os.Getenv("OIDC_AUDIENCE")),
		S3Endpoint:       env("S3_ENDPOINT", "http://minio:9000"),
		S3Region:         env("S3_REGION", "us-east-1"),
		S3AccessKey:      env("S3_ACCESS_KEY", "minio_user"),
		S3SecretKey:      os.Getenv("S3_SECRET_KEY"),
		S3Bucket:         env("S3_BUCKET", "reports"),
		CDNBaseURL:       strings.TrimRight(env("CDN_BASE_URL", "http://localhost:8083"), "/"),
		CDNSigningSecret: os.Getenv("CDN_SIGNING_SECRET"),
		CDNURLTTL:        durationEnv("CDN_URL_TTL", 24*time.Hour),
		ReportCacheTTL:   durationEnv("REPORT_CACHE_TTL", 24*time.Hour),
	}
	if cfg.DatabaseURL == "" {
		log.Fatal("OLAP_CLICKHOUSE_URL must be set")
	}
	if len([]byte(cfg.InternalSecret)) < 32 {
		log.Fatal("AUTH_INTERNAL_SECRET must contain at least 32 bytes and match bionicpro-auth")
	}
	if cfg.S3SecretKey == "" {
		log.Fatal("S3_SECRET_KEY must be set")
	}
	if len([]byte(cfg.CDNSigningSecret)) < 32 {
		log.Fatal("CDN_SIGNING_SECRET must contain at least 32 bytes")
	}

	db, err := openDB(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect to OLAP ClickHouse: %v", err)
	}
	defer db.Close()

	store, err := newS3Store(cfg)
	if err != nil {
		log.Fatalf("configure S3 object storage: %v", err)
	}
	if err := store.ensureBucket(context.Background()); err != nil {
		log.Fatalf("initialize S3 bucket: %v", err)
	}

	r := gin.New()
	r.Use(CORSMiddleware())
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/report", func(c *gin.Context) { handleReport(c, cfg, db, store) })
	r.GET("/report/days", func(c *gin.Context) { handleReportDays(c, cfg, db) })
	// These routes are intended to be reached through the reports-cdn Nginx service.
	r.GET("/internal/cdn-auth", func(c *gin.Context) { handleCDNAuth(c, cfg) })
	r.GET("/reports/*objectKey", func(c *gin.Context) { handleCDNObject(c, cfg, store) })

	log.Printf("bionicpro-reports listening on :%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}

func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", frontendOrigin)
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func handleReport(c *gin.Context, cfg Config, db *sql.DB, store *s3Store) {
	session, ok := authorizeRequest(c, cfg)
	if !ok {
		return
	}
	dateValue := strings.TrimSpace(c.Query("date"))
	if dateValue == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing_date", "message": "date query parameter is required (YYYY-MM-DD)"})
		return
	}
	reportDate, err := time.Parse("2006-01-02", dateValue)
	if err != nil || reportDate.Format("2006-01-02") != dateValue {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_date", "message": "date must be a valid date in YYYY-MM-DD format"})
		return
	}
	if strings.TrimSpace(session.Username) == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "missing_username"})
		return
	}

	manifest, cached, err := getOrCreateReport(c.Request.Context(), cfg, db, store, session.Username, reportDate)
	if err != nil {
		log.Printf("get or create report for %s: %v", dateValue, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "report_generation_failed"})
		return
	}
	cdnURL, err := createCDNURL(cfg, manifest.ObjectKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cdn_url_generation_failed"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, reportResponse{
		Date: dateValue, URL: cdnURL, Format: "csv", Cached: cached,
		Generated: manifest.GeneratedAt.UTC().Format(time.RFC3339),
	})
}

func getOrCreateReport(ctx context.Context, cfg Config, db *sql.DB, store *s3Store, username string, reportDate time.Time) (reportManifest, bool, error) {
	identityHash := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(username))))
	base := fmt.Sprintf("reports/%x/%04d/%02d/%02d", identityHash[:], reportDate.Year(), int(reportDate.Month()), reportDate.Day())
	manifestKey := base + "/latest.json"

	if raw, err := store.getObject(ctx, manifestKey); err == nil {
		var manifest reportManifest
		if json.Unmarshal(raw, &manifest) == nil && manifest.ObjectKey != "" && time.Since(manifest.GeneratedAt) < cfg.ReportCacheTTL {
			if _, headErr := store.headObject(ctx, manifest.ObjectKey); headErr == nil {
				return manifest, true, nil
			} else if !isS3NotFound(headErr) {
				return reportManifest{}, false, fmt.Errorf("check report object: %w", headErr)
			}
		}
	} else if !isS3NotFound(err) {
		return reportManifest{}, false, fmt.Errorf("read report manifest: %w", err)
	}

	csvData, err := generateReportCSV(ctx, db, username, reportDate)
	if err != nil {
		return reportManifest{}, false, err
	}
	sum := sha256.Sum256(csvData)
	checksum := hex.EncodeToString(sum[:])
	objectKey := base + "/" + checksum + ".csv"
	if err := store.putObject(ctx, objectKey, csvData, "text/csv; charset=utf-8"); err != nil {
		return reportManifest{}, false, fmt.Errorf("upload report: %w", err)
	}

	manifest := reportManifest{ObjectKey: objectKey, GeneratedAt: time.Now().UTC(), SHA256: checksum}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return reportManifest{}, false, err
	}
	if err := store.putObject(ctx, manifestKey, manifestData, "application/json"); err != nil {
		return reportManifest{}, false, fmt.Errorf("update report manifest: %w", err)
	}
	return manifest, false, nil
}

func generateReportCSV(ctx context.Context, db *sql.DB, username string, reportDate time.Time) ([]byte, error) {
	dateValue := reportDate.Format("2006-01-02")
	rows, err := db.QueryContext(ctx, `
 SELECT patient_id, patient_identity_id, first_name, last_name,
        prosthesis_id, prosthesis_model, serial_number,
        date_id, measurement_date, sensor_id, sensor_type, unit,
        measurement_count, avg_value, min_value, max_value
 FROM olap.mart_patient_telemetry_daily
 WHERE patient_identity_id = ? AND measurement_date = toDate(?)
 ORDER BY measurement_date, prosthesis_id, sensor_id`, username, dateValue)
	if err != nil {
		return nil, fmt.Errorf("query OLAP: %w", err)
	}
	defer rows.Close()

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	header := []string{"patient_id", "patient_identity_id", "first_name", "last_name", "prosthesis_id", "prosthesis_model", "serial_number", "date_id", "measurement_date", "sensor_id", "sensor_type", "unit", "measurement_count", "avg_value", "min_value", "max_value"}
	if err := w.Write(header); err != nil {
		return nil, err
	}
	for rows.Next() {
		var x reportRow
		if err := rows.Scan(&x.PatientID, &x.PatientIdentityID, &x.FirstName, &x.LastName, &x.ProsthesisID, &x.ProsthesisModel, &x.SerialNumber, &x.DateID, &x.MeasurementDate, &x.SensorID, &x.SensorType, &x.Unit, &x.MeasurementCount, &x.AvgValue, &x.MinValue, &x.MaxValue); err != nil {
			return nil, fmt.Errorf("scan report row: %w", err)
		}
		record := []string{fmt.Sprint(x.PatientID), x.PatientIdentityID, x.FirstName, x.LastName, fmt.Sprint(x.ProsthesisID), x.ProsthesisModel, x.SerialNumber, fmt.Sprint(x.DateID), x.MeasurementDate.Format("2006-01-02"), fmt.Sprint(x.SensorID), x.SensorType, x.Unit, fmt.Sprint(x.MeasurementCount), fmt.Sprintf("%.15g", x.AvgValue), fmt.Sprintf("%.15g", x.MinValue), fmt.Sprintf("%.15g", x.MaxValue)}
		if err := w.Write(record); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate report rows: %w", err)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func handleReportDays(c *gin.Context, cfg Config, db *sql.DB) {
	session, ok := authorizeRequest(c, cfg)
	if !ok {
		return
	}
	rows, err := db.QueryContext(c.Request.Context(), `
 SELECT DISTINCT measurement_date
 FROM olap.mart_patient_telemetry_daily
 WHERE patient_identity_id = ?
 ORDER BY measurement_date DESC`, session.Username)
	if err != nil {
		log.Printf("query available report dates: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "report_days_query_failed"})
		return
	}
	defer rows.Close()
	days := make([]string, 0)
	for rows.Next() {
		var day time.Time
		if err := rows.Scan(&day); err != nil {
			log.Printf("scan available report date: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "report_days_read_failed"})
			return
		}
		days = append(days, day.Format("2006-01-02"))
	}
	if err := rows.Err(); err != nil {
		log.Printf("iterate available report dates: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "report_days_read_failed"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"days": days})
}

func handleCDNAuth(c *gin.Context, cfg Config) {
	path := strings.SplitN(c.Query("path"), "?", 2)[0]
	if !validCDNSignature(cfg, path, c.Query("expires"), c.Query("signature")) {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	c.Status(http.StatusNoContent)
}

func handleCDNObject(c *gin.Context, cfg Config, store *s3Store) {
	if !validCDNSignature(cfg, c.Request.URL.Path, c.Query("expires"), c.Query("signature")) {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	key := strings.TrimPrefix(c.Request.URL.Path, "/")
	if key == "" || strings.Contains(key, "..") || strings.Contains(key, "\\") {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	data, err := store.getObject(c.Request.Context(), key)
	if err != nil {
		if isS3NotFound(err) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		log.Printf("read CDN object %q: %v", key, err)
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="telemetry-report.csv"`)
	c.Header("Cache-Control", "public, max-age=604800, immutable")
	c.Data(http.StatusOK, "text/csv; charset=utf-8", data)
}

func createCDNURL(cfg Config, objectKey string) (string, error) {
	path := "/" + objectKey
	expires := strconv.FormatInt(time.Now().Add(cfg.CDNURLTTL).Unix(), 10)
	mac := hmac.New(sha256.New, []byte(cfg.CDNSigningSecret))
	_, _ = mac.Write([]byte(path + "\n" + expires))
	signature := hex.EncodeToString(mac.Sum(nil))
	return cfg.CDNBaseURL + path + "?expires=" + url.QueryEscape(expires) + "&signature=" + url.QueryEscape(signature), nil
}

func validCDNSignature(cfg Config, path, expiresValue, signature string) bool {
	if !strings.HasPrefix(path, "/reports/") || strings.Contains(path, "..") || strings.Contains(path, "\\") {
		return false
	}
	expires, err := strconv.ParseInt(expiresValue, 10, 64)
	if err != nil || expires <= time.Now().Unix() || expires > time.Now().Add(7*24*time.Hour).Unix() {
		return false
	}
	mac := hmac.New(sha256.New, []byte(cfg.CDNSigningSecret))
	_, _ = mac.Write([]byte(path + "\n" + expiresValue))
	expected, err := hex.DecodeString(signature)
	return err == nil && hmac.Equal(mac.Sum(nil), expected)
}

func authorizeRequest(c *gin.Context, cfg Config) (authSession, bool) {
	sid, err := c.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(sid) == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return authSession{}, false
	}
	session, rotatedCookie, status, err := validateAndRotate(c.Request.Context(), cfg, sid)
	if rotatedCookie != "" {
		c.Header("Set-Cookie", rotatedCookie)
	}
	if err != nil {
		if status == 0 {
			status = http.StatusBadGateway
		}
		c.JSON(status, gin.H{"error": "session_validation_failed"})
		return authSession{}, false
	}
	if session.UserID == "" || session.AccessToken == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid_auth_response"})
		return authSession{}, false
	}
	claims, err := validateAccessToken(c.Request.Context(), cfg, session.AccessToken)
	if err != nil {
		log.Printf("access token validation failed: %v", err)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_access_token"})
		return authSession{}, false
	}
	if claims.Subject != session.UserID {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "session_user_mismatch"})
		return authSession{}, false
	}
	if !hasRole(claims, "prothetic_user") {
		log.Printf("user %q has no prosthetic_user role", session.Username)
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return authSession{}, false
	}
	return session, true
}

func validateAndRotate(ctx context.Context, cfg Config, sid string) (authSession, string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.AuthURL+"/internal/session/validate-and-rotate", nil)
	if err != nil {
		return authSession{}, "", 0, err
	}
	req.Header.Set("X-Internal-Auth", cfg.InternalSecret)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	resp, err := httpClient.Do(req)
	if err != nil {
		return authSession{}, "", 0, err
	}
	defer resp.Body.Close()
	cookie := ""
	for _, v := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(v, sessionCookieName+"=") {
			cookie = v
			break
		}
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return authSession{}, cookie, resp.StatusCode, fmt.Errorf("auth service returned %s", resp.Status)
	}
	var result authSession
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return authSession{}, cookie, http.StatusBadGateway, err
	}
	return result, cookie, http.StatusOK, nil
}

func validateAccessToken(ctx context.Context, cfg Config, raw string) (jwtClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return jwtClaims{}, errors.New("malformed JWT")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jwtClaims{}, err
	}
	var h jwtHeader
	if err := json.Unmarshal(hb, &h); err != nil || h.Alg != "RS256" || h.Kid == "" {
		return jwtClaims{}, errors.New("unsupported JWT header")
	}
	key, err := fetchSigningKey(ctx, cfg.OIDCIssuer+"/protocol/openid-connect/certs", h.Kid)
	if err != nil {
		return jwtClaims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jwtClaims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return jwtClaims{}, errors.New("JWT signature verification failed")
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtClaims{}, err
	}
	var claims jwtClaims
	if err := json.Unmarshal(cb, &claims); err != nil {
		return jwtClaims{}, err
	}
	now := time.Now().Unix()
	if claims.Issuer != cfg.OIDCIssuer || claims.Subject == "" || claims.ExpiresAt <= now || (claims.NotBefore != 0 && claims.NotBefore > now) {
		return jwtClaims{}, errors.New("JWT claims validation failed")
	}
	if cfg.OIDCAudience != "" && !audienceContains(claims.Audience, cfg.OIDCAudience) {
		return jwtClaims{}, errors.New("JWT audience validation failed")
	}
	return claims, nil
}

func fetchSigningKey(ctx context.Context, jwksURL, kid string) (*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS endpoint returned %s", resp.Status)
	}
	var set jwksResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, err
	}
	for _, k := range set.Keys {
		if k.Kid != kid || k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, err
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, err
		}
		e := new(big.Int).SetBytes(eb)
		if !e.IsInt64() || e.Int64() < 3 {
			return nil, errors.New("invalid RSA exponent")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(e.Int64())}, nil
	}
	return nil, errors.New("matching signing key not found")
}

func audienceContains(raw json.RawMessage, expected string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == expected
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, v := range many {
			if v == expected {
				return true
			}
		}
	}
	return false
}

func hasRole(c jwtClaims, role string) bool {
	for _, r := range c.RealmAccess.Roles {
		if r == role {
			return true
		}
	}
	for _, client := range c.ResourceAccess {
		for _, r := range client.Roles {
			if r == role {
				return true
			}
		}
	}
	return false
}

func newS3Store(cfg Config) (*s3Store, error) {
	endpoint, err := url.Parse(cfg.S3Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, fmt.Errorf("invalid S3_ENDPOINT %q", cfg.S3Endpoint)
	}
	return &s3Store{endpoint: endpoint, region: cfg.S3Region, accessKey: cfg.S3AccessKey, secretKey: cfg.S3SecretKey, bucket: cfg.S3Bucket, client: &http.Client{Timeout: 45 * time.Second}}, nil
}

func (s *s3Store) ensureBucket(ctx context.Context) error {
	var lastErr error
	for attempt := 1; attempt <= 30; attempt++ {
		resp, err := s.do(ctx, http.MethodHead, s.bucket, nil, "")
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				return nil
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				lastErr = fmt.Errorf("HEAD bucket returned %s", resp.Status)
				time.Sleep(2 * time.Second)
				continue
			}
			createResp, createErr := s.do(ctx, http.MethodPut, s.bucket, nil, "")
			if createErr == nil {
				body, _ := io.ReadAll(io.LimitReader(createResp.Body, 4096))
				createResp.Body.Close()
				if createResp.StatusCode == http.StatusOK || createResp.StatusCode == http.StatusNoContent || createResp.StatusCode == http.StatusConflict {
					return nil
				}
				lastErr = fmt.Errorf("create bucket returned %s: %s", createResp.Status, strings.TrimSpace(string(body)))
			} else {
				lastErr = createErr
			}
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("bucket %q is not ready: %w", s.bucket, lastErr)
}

func (s *s3Store) putObject(ctx context.Context, key string, data []byte, contentType string) error {
	resp, err := s.do(ctx, http.MethodPut, s.bucket+"/"+key, data, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.responseError(resp)
	}
	return nil
}

func (s *s3Store) getObject(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.do(ctx, http.MethodGet, s.bucket+"/"+key, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, s.responseError(resp)
	}
	return io.ReadAll(resp.Body)
}

func (s *s3Store) headObject(ctx context.Context, key string) (time.Time, error) {
	resp, err := s.do(ctx, http.MethodHead, s.bucket+"/"+key, nil, "")
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return time.Time{}, s.responseError(resp)
	}
	lastModified, err := http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil {
		return time.Time{}, nil
	}
	return lastModified, nil
}

func (s *s3Store) responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var x struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(body, &x)
	if resp.StatusCode == http.StatusNotFound || x.Code == "NoSuchKey" || x.Code == "NoSuchBucket" {
		return fmt.Errorf("s3 object not found (%s): %s", resp.Status, x.Code)
	}
	return fmt.Errorf("S3 returned %s: %s %s", resp.Status, x.Code, x.Message)
}

func isS3NotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "s3 object not found")
}

func (s *s3Store) do(ctx context.Context, method, key string, body []byte, contentType string) (*http.Response, error) {
	objectPath := strings.TrimRight(s.endpoint.Path, "/") + "/" + key
	u := *s.endpoint
	u.Path = objectPath
	u.RawPath = ""
	payloadHash := sha256.Sum256(body)
	payloadHex := hex.EncodeToString(payloadHash[:])
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-amz-content-sha256", payloadHex)
	req.Header.Set("x-amz-date", amzDate)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	canonicalHeaders := ""
	signedHeaders := ""
	if contentType != "" {
		canonicalHeaders = "content-type:" + strings.TrimSpace(contentType) + "\n"
		signedHeaders = "content-type;"
	}
	canonicalHeaders += "host:" + req.URL.Host + "\n" + "x-amz-content-sha256:" + payloadHex + "\n" + "x-amz-date:" + amzDate + "\n"
	signedHeaders += "host;x-amz-content-sha256;x-amz-date"
	canonicalURI := req.URL.EscapedPath()
	canonicalQuery := canonicalizeQuery(req.URL.Query())
	canonicalRequest := method + "\n" + canonicalURI + "\n" + canonicalQuery + "\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + payloadHex
	canonicalHash := sha256.Sum256([]byte(canonicalRequest))
	scope := dateStamp + "/" + s.region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])
	signingKey := s3SigningKey(s.secretKey, dateStamp, s.region)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.accessKey+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
	return s.client.Do(req)
}

func canonicalizeQuery(values url.Values) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), values[k]...)
		sort.Strings(vs)
		if len(vs) == 0 {
			parts = append(parts, url.QueryEscape(k)+"=")
			continue
		}
		for _, v := range vs {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func awsEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(url.QueryEscape(s), "+", "%20"), "%7E", "~")
}

func s3SigningKey(secret, date, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
func durationEnv(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Fatalf("%s must be a positive duration (e.g. 24h): %v", key, err)
	}
	return d
}

// Keep crypto/rand linked in builds where deployments replace the local signing
// secret with a generated one; this helper is also useful for local secret setup.
