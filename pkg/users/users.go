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

package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

type httpServer struct {
	redisClient *redis.Client
}

type User struct {
	Name string `json:"name"`
	Rpm  int64  `json:"rpm"`
	Tpm  int64  `json:"tpm"`
}

func NewHTTPServer(addr string, redis *redis.Client) *http.Server {
	server := &httpServer{
		redisClient: redis,
	}
	r := mux.NewRouter()
	r.HandleFunc("/CreateUser", server.createUser).Methods("POST")
	r.HandleFunc("/ReadUser", server.readUser).Methods("POST")
	r.HandleFunc("/UpdateUser", server.updateUser).Methods("POST")
	r.HandleFunc("/DeleteUser", server.deleteUser).Methods("POST")

	return &http.Server{
		Addr:    addr,
		Handler: r,
	}
}

func (s *httpServer) createUser(w http.ResponseWriter, r *http.Request) {
	var u User

	err := decodeJSONBody(w, r, &u)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			log.Print(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	if s.checkUser(u) {
		fmt.Fprintf(w, "User: %+v exists", u.Name)
		return
	}

	if err := s.setUser(u); err != nil {
		fmt.Fprintf(w, "error occurred on creating user: %+v", err)
		return
	}

	fmt.Fprintf(w, "Created User: %+v", u)
}

func (s *httpServer) readUser(w http.ResponseWriter, r *http.Request) {
	var u User

	err := decodeJSONBody(w, r, &u)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			log.Print(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	user, err := s.getUser(u)
	if err != nil {
		fmt.Fprint(w, "user does not exists")
		return
	}

	fmt.Fprintf(w, "User: %+v", user)
}

func (s *httpServer) updateUser(w http.ResponseWriter, r *http.Request) {
	var u User

	err := decodeJSONBody(w, r, &u)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			log.Print(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	if !s.checkUser(u) {
		fmt.Fprintf(w, "User: %+v does not exists", u.Name)
		return
	}

	if err := s.setUser(u); err != nil {
		fmt.Fprintf(w, "error occurred on updating user: %+v", err)
		return
	}

	fmt.Fprintf(w, "Updated User: %+v", u)
}

func (s *httpServer) deleteUser(w http.ResponseWriter, r *http.Request) {
	var u User

	err := decodeJSONBody(w, r, &u)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			log.Print(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	if !s.checkUser(u) {
		fmt.Fprintf(w, "User: %+v does not exists", u.Name)
		return
	}

	if err := s.delUser(u); err != nil {
		fmt.Fprintf(w, "error occurred on deleting user: %+v", err)
		return
	}

	fmt.Fprintf(w, "Deleted User: %+v", u)
}

func (s *httpServer) checkUser(u User) bool {
	val, err := s.redisClient.Exists(context.Background(), genKey(u.Name)).Result()
	if err != nil {
		return false
	}

	return val != 0
}

func (s *httpServer) getUser(u User) (User, error) {
	val, err := s.redisClient.Get(context.Background(), genKey(u.Name)).Result()
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

func (s *httpServer) setUser(u User) error {
	b, err := json.Marshal(&u)
	if err != nil {
		return err
	}

	return s.redisClient.Set(context.Background(), genKey(u.Name), string(b), 0).Err()
}

func (s *httpServer) delUser(u User) error {
	return s.redisClient.Del(context.Background(), genKey(u.Name)).Err()
}

func genKey(s string) string {
	return fmt.Sprintf("aibrix-users/%s", s)
}
