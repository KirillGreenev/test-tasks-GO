// Написать мини сервис с разделением слоев в одном main.go файле. Сервис должен уметь:
// 1. Подключаться к базе данных
// 2. Использовать кэш c применением Proxy паттерна
// 3. Принимать http запросы REST like API
// 4. Регистрировать пользователя в базе данных
// 5. Выводить список всех пользователей
// 6. У пользователя следующие данные email, password, name, age
// 7. Запретить регистрацию пользователей с одинаковым email и возрастом меньше 18 лет

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

// models
type User struct {
	ID       int    `json:"id" db:"id"`
	Email    string `json:"email" db:"email"`
	Password string `json:"password" db:"password"`
	Name     string `json:"name" db:"name"`
	Age      int    `json:"age" db:"age"`
}

// repository
type UserRepository interface {
	Create(ctx context.Context, user *User) error
	GetAll(ctx context.Context) ([]User, error)
}

type UserRepositoryImpl struct {
	db *sqlx.DB
}

func NewUserRepositoryImpl(db *sqlx.DB) *UserRepositoryImpl {
	return &UserRepositoryImpl{db: db}
}

func initDB() (*sqlx.DB, error) {
	db, err := sqlx.Open("sqlite3", "./users.db")
	if err != nil {
		return nil, err
	}

	createTableSQL := `CREATE TABLE IF NOT EXISTS users (
                                     id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
                                     email VARCHAR(100) NOT NULL UNIQUE,
                                     name VARCHAR(100) NOT NULL,
                                     age INTEGER,
                                     password VARCHAR(100) NOT NULL
);`

	if _, err := db.Exec(createTableSQL); err != nil {
		return nil, err
	}

	return db, nil
}

func (repo *UserRepositoryImpl) Create(ctx context.Context, user *User) error {
	_, err := repo.db.ExecContext(ctx, "INSERT INTO users (email, password, name, age) VALUES (?, ?, ?, ?)",
		user.Email, user.Password, user.Name, user.Age)
	return err
}

func (repo *UserRepositoryImpl) GetAll(ctx context.Context) ([]User, error) {
	var users []User
	err := repo.db.SelectContext(ctx, &users, "SELECT * FROM users")
	return users, err
}

type CacheProxy struct {
	repo        UserRepository
	cache       map[int]User
	countUserDb int
}

func NewCacheProxy(repo UserRepository) *CacheProxy {
	return &CacheProxy{
		repo:        repo,
		cache:       make(map[int]User),
		countUserDb: -100,
	}
}

func (cp *CacheProxy) Create(ctx context.Context, user *User) error {
	cp.countUserDb++
	return cp.repo.Create(ctx, user)

}
func (cp *CacheProxy) GetAll(ctx context.Context) ([]User, error) {
	if len(cp.cache) == cp.countUserDb {
		users := make([]User, 0, len(cp.cache))
		for _, user := range cp.cache {
			users = append(users, user)
		}
		return users, nil
	}

	users, err := cp.repo.GetAll(ctx)
	if err != nil {
		return nil, err
	}

	cp.countUserDb = len(users)
	for _, user := range users {
		cp.cache[user.ID] = user
	}

	return users, nil
}

// Service
type UserService interface {
	Create(ctx context.Context, user *User) error
	GetAll(ctx context.Context) ([]User, error)
}

type UserServiceImpl struct {
	repo UserRepository
}

func (u *UserServiceImpl) Create(ctx context.Context, user *User) error {
	if user.Age < 18 {
		return fmt.Errorf("Age under 18, registration prohibited")
	}
	return u.repo.Create(ctx, user)
}

func (u *UserServiceImpl) GetAll(ctx context.Context) ([]User, error) {
	return u.repo.GetAll(ctx)
}

func NewUserServiceImpl(repo UserRepository) *UserServiceImpl {
	return &UserServiceImpl{repo: repo}
}

// Controller
type ControllerUser struct {
	UserService UserService
}

func NewControllerUser(userService UserService) *ControllerUser {
	return &ControllerUser{UserService: userService}
}

func (c *ControllerUser) RegisterHandler(w http.ResponseWriter, r *http.Request) {
	var user User
	if err := json.NewDecoder(r.Body).Decode(&user); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	reqCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := c.UserService.Create(reqCtx, &user); err != nil {
		log.Println("ControllerUser.RegisterHandler error: ", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fmt.Fprint(w, "User successfully registered")
}

func (c *ControllerUser) GetUsersHandler(w http.ResponseWriter, r *http.Request) {
	reqCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	user, err := c.UserService.GetAll(reqCtx)
	if err != nil {
		log.Println("ControllerUser.GetUsersHandler error: ", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(user)
}

func main() {
	db, err := initDB()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()

	controller := NewControllerUser(NewUserServiceImpl(NewCacheProxy(NewUserRepositoryImpl(db))))

	r := chi.NewRouter()

	r.Post("/user", controller.RegisterHandler)
	r.Get("/user", controller.GetUsersHandler)

	server := &http.Server{
		Addr:         ":8080",
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Println("Starting server on :8080...")
		if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	<-stop

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err = server.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown error: %v", err)
	}

	log.Println("Server stopped gracefully")
}
