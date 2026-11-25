package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/scrypt"
)

const sessionCookieName = "acestreamhub_session"

func secretBytes(hexKey string) []byte {
	if b, err := hex.DecodeString(hexKey); err == nil && len(b) > 0 {
		return b
	}
	return []byte(hexKey)
}

func signSessionToken(secret string, issuedAt int64) string {
	payload := strconv.FormatInt(issuedAt, 10)
	mac := hmac.New(sha256.New, secretBytes(secret))
	_, _ = mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return payload + "." + sig
}

func verifySessionToken(secret, token string) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	issuedAt, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	if time.Since(time.Unix(issuedAt, 0)) > 30*24*time.Hour {
		return false
	}
	gotSig, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secretBytes(secret))
	_, _ = mac.Write([]byte(parts[0]))
	wantSig := mac.Sum(nil)
	return subtle.ConstantTimeCompare(gotSig, wantSig) == 1
}

func setSessionCookie(w http.ResponseWriter, secret string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    signSessionToken(secret, time.Now().Unix()),
		Path:     "/",
		MaxAge:   30 * 24 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func isAuthenticated(r *http.Request, secret string) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	return verifySessionToken(secret, c.Value)
}

// HashPassword hashes a plaintext password with bcrypt.
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword verifies bcrypt hashes as well as Python Werkzeug pbkdf2:sha256 and scrypt hashes.
func CheckPassword(storedHash, password string) bool {
	if storedHash == "" {
		return false
	}
	if strings.HasPrefix(storedHash, "$2") {
		return bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(password)) == nil
	}
	if strings.HasPrefix(storedHash, "pbkdf2:sha256:") {
		return checkWerkzeugPBKDF2(storedHash, password)
	}
	if strings.HasPrefix(storedHash, "scrypt:") {
		return checkWerkzeugScrypt(storedHash, password)
	}
	return false
}

func checkWerkzeugPBKDF2(stored, password string) bool {
	parts := strings.SplitN(stored, "$", 3)
	if len(parts) != 3 {
		return false
	}
	meta := strings.Split(parts[0], ":")
	iter := 600000
	if len(meta) == 3 {
		if n, err := strconv.Atoi(meta[2]); err == nil && n > 0 {
			iter = n
		}
	}
	wantHex := parts[2]
	wantBytes, err := hex.DecodeString(wantHex)
	if err != nil || len(wantBytes) == 0 {
		return false
	}
	derived := pbkdf2.Key([]byte(password), []byte(parts[1]), iter, len(wantBytes), sha256.New)
	return subtle.ConstantTimeCompare(derived, wantBytes) == 1
}

func checkWerkzeugScrypt(stored, password string) bool {
	parts := strings.SplitN(stored, "$", 3)
	if len(parts) != 3 {
		return false
	}
	meta := strings.Split(strings.TrimPrefix(parts[0], "scrypt:"), ":")
	if len(meta) != 3 {
		return false
	}
	n, err1 := strconv.Atoi(meta[0])
	r, err2 := strconv.Atoi(meta[1])
	p, err3 := strconv.Atoi(meta[2])
	if err1 != nil || err2 != nil || err3 != nil || n <= 1 || r <= 0 || p <= 0 {
		return false
	}
	wantBytes, err := hex.DecodeString(parts[2])
	if err != nil || len(wantBytes) == 0 {
		return false
	}
	derived, err := scrypt.Key([]byte(password), []byte(parts[1]), n, r, p, len(wantBytes))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(derived, wantBytes) == 1
}

func first8(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

func last4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

func fmtFloat1(v float64) string {
	return fmt.Sprintf("%.1f", v)
}
