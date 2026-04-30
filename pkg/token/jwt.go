package token

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWTManager 负责生成和校验 JWT。
// 它保存签名密钥、access token 有效期、refresh token 有效期。
type JWTManager struct {
	secretKey       []byte
	accessTokenDur  time.Duration
	refreshTokenDur time.Duration
}

// CustomClaims 是项目自定义的 JWT payload。
// RegisteredClaims 是 JWT 标准字段，比如过期时间、签发时间、生效时间。
type CustomClaims struct {
	UserID   uint   `json:"userId"`
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// NewJWTManager 创建 JWT 管理器。
// accessTokenExpireHours 单位是小时。
// refreshTokenExpireDays 单位是天。
func NewJWTManager(secret string, accessTokenExpireHours, refreshTokenExpireDays int) *JWTManager {
	return &JWTManager{
		secretKey:       []byte(secret),
		accessTokenDur:  time.Hour * time.Duration(accessTokenExpireHours),
		refreshTokenDur: time.Duration(refreshTokenExpireDays) * 24 * time.Hour,
	}
}

// GenerateToken 生成 access token。
// access token 有效期短，用于日常接口鉴权。
func (m *JWTManager) GenerateToken(userID uint, username, role string) (string, error) {
	claims := CustomClaims{
		UserID:   userID,
		Username: username,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(m.accessTokenDur)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(m.secretKey)
}

// GenerateRefreshToken 生成 refresh token。
// refresh token 有效期更长，用于刷新 access token。
func (m *JWTManager) GenerateRefreshToken(userID uint, username, role string) (string, error) {
	claims := CustomClaims{
		UserID:   userID,
		Username: username,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(m.refreshTokenDur)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(m.secretKey)
}

// VerifyToken 校验 token，并解析出 claims。
// 签名错误、过期、格式错误都会返回 error。
func (m *JWTManager) VerifyToken(tokenString string) (*CustomClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &CustomClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}

		return m.secretKey, nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*CustomClaims); ok && token.Valid {
		return claims, nil
	}

	return nil, errors.New("invalid token")
}

// GenerateRandomString 生成随机字符串。
// 后面 WebSocket stop token 等临时 token 场景会用到。
func GenerateRandomString(length int) string {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("fallback%d", time.Now().UnixNano())
	}

	return hex.EncodeToString(bytes)
}
