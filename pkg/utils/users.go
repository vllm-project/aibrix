package utils

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
)

type User struct {
	Name string `json:"name"`
	Rpm  int64  `json:"rpm"`
	Tpm  int64  `json:"tpm"`
}

func CheckUser(u User, redisClient *redis.Client) bool {
	val, err := redisClient.Exists(context.Background(), genKey(u.Name)).Result()
	if err != nil {
		return false
	}

	return val != 0
}

func GetUser(u User, redisClient *redis.Client) (User, error) {
	val, err := redisClient.Get(context.Background(), genKey(u.Name)).Result()
	if err != nil {
		return User{}, err
	}
	user := &User{}
	err = json.Unmarshal([]byte(val), user)
	if err != nil {
		return User{}, err
	}

	return *user, nil
}

func SetUser(u User, redisClient *redis.Client) error {
	b, err := json.Marshal(&u)
	if err != nil {
		return err
	}

	return redisClient.Set(context.Background(), genKey(u.Name), string(b), 0).Err()
}

func DelUser(u User, redisClient *redis.Client) error {
	return redisClient.Del(context.Background(), genKey(u.Name)).Err()
}

func genKey(s string) string {
	return fmt.Sprintf("aibrix-users/%s", s)
}
