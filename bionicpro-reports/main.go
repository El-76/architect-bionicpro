package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/gin-gonic/gin"
)

const sessionCookieName = "session_id"
const frontendOrigin = "http://192.168.56.106:3000"

var httpClient = &http.Client{Timeout: 10 * time.Second}

type Config struct {
	Port           string
	DatabaseURL    string
	AuthURL        string
	InternalSecret string
	OIDCIssuer     string
	OIDCAudience   string
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
        PreferredUsername string `json:"preferred_username"`
	Audience          json.RawMessage `json:"aud"`
	ExpiresAt         int64           `json:"exp"`
	NotBefore         int64           `json:"nbf"`
	RealmAccess struct {
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

func main() {
	cfg := Config{
		Port:           env("PORT", "8082"),
		DatabaseURL:    strings.TrimSpace(os.Getenv("OLAP_CLICKHOUSE_URL")),
		AuthURL:        strings.TrimRight(env("AUTH_INTERNAL_URL", "http://bionicpro-auth:8081"), "/"),
		InternalSecret: os.Getenv("AUTH_INTERNAL_SECRET"),
		OIDCIssuer:     strings.TrimRight(env("OIDC_ISSUER", "http://192.168.56.106:8080/realms/reports-realm"), "/"),
		OIDCAudience:   env("OIDC_AUDIENCE", "bionicpro-auth"),
	}
	if cfg.DatabaseURL == "" {
		log.Fatal("OLAP_CLICKHOUSE_URL must be set")
	}
	if len([]byte(cfg.InternalSecret)) < 32 {
		log.Fatal("AUTH_INTERNAL_SECRET must contain at least 32 bytes and match bionicpro-auth")
	}

	db, err := openDB(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect to OLAP ClickHouse: %v", err)
	}
	defer db.Close()

	r := gin.New()
	r.Use(CORSMiddleware())
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/report", func(c *gin.Context) { handleReport(c, cfg, db) })
	r.GET("/report/days", func(c *gin.Context) { handleReportDays(c, cfg, db) })
	log.Printf("bionicpro-reports listening on :%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
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

func handleReport(c *gin.Context, cfg Config, db *sql.DB) {
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

	rows, err := db.QueryContext(c.Request.Context(), `
 SELECT patient_id, patient_identity_id, first_name, last_name,
        prosthesis_id, prosthesis_model, serial_number,
        date_id, measurement_date, sensor_id, sensor_type, unit,
        measurement_count, avg_value, min_value, max_value
 FROM olap.mart_patient_telemetry_daily
 WHERE patient_identity_id = ? AND measurement_date = toDate(?)
 ORDER BY measurement_date, prosthesis_id, sensor_id`, session.Username, dateValue)
	if err != nil {
		log.Printf("query OLAP ClickHouse report: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "report_query_failed"})
		return
	}
	defer rows.Close()
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="patient-telemetry-report-%s.csv"`, dateValue))
	c.Header("Cache-Control", "no-store")
	w := csv.NewWriter(c.Writer)
	header := []string{"patient_id", "patient_identity_id", "first_name", "last_name", "prosthesis_id", "prosthesis_model", "serial_number", "date_id", "measurement_date", "sensor_id", "sensor_type", "unit", "measurement_count", "avg_value", "min_value", "max_value"}
	if err := w.Write(header); err != nil {
		return
	}
	for rows.Next() {
		var x reportRow
		if err := rows.Scan(&x.PatientID, &x.PatientIdentityID, &x.FirstName, &x.LastName, &x.ProsthesisID, &x.ProsthesisModel, &x.SerialNumber, &x.DateID, &x.MeasurementDate, &x.SensorID, &x.SensorType, &x.Unit, &x.MeasurementCount, &x.AvgValue, &x.MinValue, &x.MaxValue); err != nil {
			log.Printf("scan report row: %v", err)
			return
		}
		record := []string{fmt.Sprint(x.PatientID), x.PatientIdentityID, x.FirstName, x.LastName, fmt.Sprint(x.ProsthesisID), x.ProsthesisModel, x.SerialNumber, fmt.Sprint(x.DateID), x.MeasurementDate.Format("2006-01-02"), fmt.Sprint(x.SensorID), x.SensorType, x.Unit, fmt.Sprint(x.MeasurementCount), fmt.Sprintf("%.15g", x.AvgValue), fmt.Sprintf("%.15g", x.MinValue), fmt.Sprintf("%.15g", x.MaxValue)}
		if err := w.Write(record); err != nil {
			return
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("iterate report rows: %v", err)
		return
	}
	w.Flush()
	if err := w.Error(); err != nil {
		log.Printf("write report CSV: %v", err)
	}
}

// handleReportDays returns the dates for which the authenticated patient has telemetry rows.
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

// authorizeRequest validates and rotates the browser session, verifies the access token,
// and enforces the prosthetic_user role. Both report endpoints use the same gate.
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
                log.Printf("no role in claims: %+v", claims)
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
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
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
	log.Printf("Claims: %+v", claims)
	if claims.Issuer != cfg.OIDCIssuer || claims.Subject == "" || claims.ExpiresAt <= now || (claims.NotBefore != 0 && claims.NotBefore > now) {
		return jwtClaims{}, errors.New("JWT claims validation failed")
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
func env(k, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return fallback
}
