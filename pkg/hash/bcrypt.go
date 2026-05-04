package hash

import "golang.org/x/crypto/bcrypt"

// HashPassword 使用 bcrypt 对明文密码做哈希。
// 数据库里永远不应该保存明文密码。
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

// CheckPasswordHash 校验用户输入的明文密码和数据库里的哈希值是否匹配。
func CheckPasswordHash(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}
