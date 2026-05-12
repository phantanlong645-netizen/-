package storagepath

import "fmt"

func MergedObjectName(userID uint, fileMD5, fileName string) string {
	return fmt.Sprintf("merged/%d/%s/%s", userID, fileMD5, fileName)
}
