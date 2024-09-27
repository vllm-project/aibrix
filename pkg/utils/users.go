/*
Copyright 2024 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
)

type User struct {
	Name string `json:"name" validate:"required"`
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
	if u.Rpm < 0 || u.Tpm < 0 {
		return fmt.Errorf("rpm or tpm can not negative")
	}

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
