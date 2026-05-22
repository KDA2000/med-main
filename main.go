package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	_ "github.com/lib/pq"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── Глобальные переменные ───────────────────────────────────────────────────

var (
	db          *sql.DB
	redisClient *redis.Client
	mongoDB     *mongo.Database
)

// ─── Структуры ───────────────────────────────────────────────────────────────

type Doctor struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	Experience    string `json:"experience"`
	SpecialtyName string `json:"specialty_name"`
	SpecKey       string `json:"spec_key"` // значение для <select> на странице записи
}

// specNameToKey переводит название специальности из БД
// в значение option в <select id="specialization">
func specNameToKey(name string) string {
	switch name {
	case "Терапия":
		return "Терапевт"
	case "Кардиология":
		return "Кардиолог"
	case "Неврология":
		return "Невролог"
	default:
		return name
	}
}

type AppointmentRequest struct {
	Name       string `json:"name"`
	Phone      string `json:"phone"`
	Email      string `json:"email"`
	Spec       string `json:"spec"`
	DoctorID   int    `json:"doctor_id"`
	DoctorName string `json:"doctor_name"`
	Date       string `json:"date"` // "2026-05-12"
	Time       string `json:"time"` // "14:00"
}

// Для страницы администратора
type AdminAppointment struct {
	ID          int    `json:"id"`
	PatientName string `json:"patient_name"`
	Phone       string `json:"phone"`
	Email       string `json:"email"`
	DoctorName  string `json:"doctor_name"`
	Specialty   string `json:"specialty"`
	SlotDate    string `json:"slot_date"`
	SlotTime    string `json:"slot_time"`
}

type AdminPageData struct {
	Appointments       []AdminAppointment
	OnlineAppointments []AdminAppointment
}

// Для MongoDB-отзывов
type Review struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	Name      string             `bson:"name"          json:"name"`
	Text      string             `bson:"text"          json:"text"`
	Rating    int                `bson:"rating"        json:"rating"`
	CreatedAt time.Time          `bson:"created_at"    json:"created_at"`
}

// ─── Инициализация ────────────────────────────────────────────────────────────

func initDB() {
	connStr := "user=postgres password=password dbname=med_db host=localhost port=5433 sslmode=disable"
	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal(err)
	}
	if err = db.Ping(); err != nil {
		log.Fatal("БД недоступна:", err)
	}
	fmt.Println("PostgreSQL: соединение установлено (порт 5433)")
}

func initRedis() {
	redisClient = redis.NewClient(&redis.Options{
		Addr:     "localhost:6379", // стандартный порт Redis
		Password: "",               // без пароля по умолчанию
		DB:       0,
	})
	ctx := context.Background()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Printf("⚠️  Redis недоступен: %v — продолжаем без кэша", err)
		redisClient = nil
		return
	}
	fmt.Println("Redis: соединение установлено (порт 6379)")
}

func initMongo() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Подключаемся к MongoDB (стандартный порт 27017)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		log.Printf("⚠️  MongoDB недоступна: %v — отзывы будут недоступны", err)
		return
	}
	if err := client.Ping(ctx, nil); err != nil {
		log.Printf("⚠️  MongoDB ping провалился: %v", err)
		return
	}

	mongoDB = client.Database("med_db") // база данных "med_db"
	fmt.Println("MongoDB: соединение установлено (порт 27017)")
}

// ─── Шаблоны ─────────────────────────────────────────────────────────────────

func render(w http.ResponseWriter, tmplName string, data interface{}) {
	// Функция inc для шаблона (нумерация строк: {{ inc $i }})
	funcMap := template.FuncMap{
		"inc": func(i int) int { return i + 1 },
	}
	tmpl, err := template.New("").Funcs(funcMap).ParseFiles(
		"templates/header.html",
		"templates/"+tmplName,
		"templates/footer.html",
	)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	tmpl.ExecuteTemplate(w, tmplName, data)
}

// ─── Страницы ─────────────────────────────────────────────────────────────────

func homeHandler(w http.ResponseWriter, r *http.Request)    { render(w, "home.html", nil) }
func bookingHandler(w http.ResponseWriter, r *http.Request) { render(w, "booking.html", nil) }
func onlineHandler(w http.ResponseWriter, r *http.Request)  { render(w, "online.html", nil) }

func doctorsPageHandler(w http.ResponseWriter, r *http.Request) {
	rows, _ := db.Query(`SELECT d.id, d.name, d.experience, s.name FROM doctors d JOIN specialties s ON d.specialty_id = s.id`)
	defer rows.Close()
	var doctors []Doctor
	for rows.Next() {
		var d Doctor
		rows.Scan(&d.ID, &d.Name, &d.Experience, &d.SpecialtyName)
		d.SpecKey = specNameToKey(d.SpecialtyName) // ← заполняем правильное значение для select
		doctors = append(doctors, d)
	}
	render(w, "doctors.html", doctors)
}

// ─── Панель администратора (с Redis-кэшем) ────────────────────────────────────

const adminCacheKey = "admin:page:data"
const adminCacheTTL = 1 * time.Hour

func adminHandler(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	forceRefresh := r.URL.Query().Get("refresh") == "1"

	// 1. Сброс кэша по запросу
	if forceRefresh && redisClient != nil {
		redisClient.Del(ctx, adminCacheKey)
	}

	// 2. Пробуем взять из Redis
	if redisClient != nil && !forceRefresh {
		cached, err := redisClient.Get(ctx, adminCacheKey).Bytes()
		if err == nil {
			var data AdminPageData
			if json.Unmarshal(cached, &data) == nil {
				render(w, "admin.html", data)
				return
			}
		}
	}

	// 3. Грузим из PostgreSQL
	data := AdminPageData{}

	// Очные записи
	rows, err := db.Query(`
		SELECT id, COALESCE(patient_name,''), COALESCE(phone,''), COALESCE(email,''),
		       COALESCE(doctor_name,''), COALESCE(specialty,''),
		       TO_CHAR(slot_date,'YYYY-MM-DD'), TO_CHAR(slot_time,'HH24:MI')
		FROM appointments ORDER BY slot_date DESC, slot_time DESC`)
	if err != nil {
		log.Printf("Ошибка чтения appointments: %v", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var a AdminAppointment
			rows.Scan(&a.ID, &a.PatientName, &a.Phone, &a.Email, &a.DoctorName, &a.Specialty, &a.SlotDate, &a.SlotTime)
			data.Appointments = append(data.Appointments, a)
		}
	}

	// Онлайн-записи
	rows2, err := db.Query(`
		SELECT COALESCE(patient_name,''), COALESCE(phone,''), COALESCE(email,''),
		       COALESCE(doctor_name,''), COALESCE(specialty,''),
		       TO_CHAR(slot_date,'YYYY-MM-DD'), TO_CHAR(slot_time,'HH24:MI')
		FROM online_appointments ORDER BY slot_date DESC, slot_time DESC`)
	if err != nil {
		log.Printf("Ошибка чтения online_appointments: %v", err)
	} else {
		defer rows2.Close()
		for rows2.Next() {
			var a AdminAppointment
			rows2.Scan(&a.PatientName, &a.Phone, &a.Email, &a.DoctorName, &a.Specialty, &a.SlotDate, &a.SlotTime)
			data.OnlineAppointments = append(data.OnlineAppointments, a)
		}
	}

	// 4. Сохраняем в Redis на 1 час
	if redisClient != nil {
		if b, err := json.Marshal(data); err == nil {
			redisClient.Set(ctx, adminCacheKey, b, adminCacheTTL)
		}
	}

	render(w, "admin.html", data)
}

// ─── API ──────────────────────────────────────────────────────────────────────

func getDoctorsHandler(w http.ResponseWriter, r *http.Request) {
	specName := r.URL.Query().Get("spec")
	shortSpec := ""
	if len(specName) > 4 {
		shortSpec = strings.ToLower(specName[:4])
	} else {
		shortSpec = strings.ToLower(specName)
	}

	rows, err := db.Query(`
        SELECT d.id, d.name, d.experience 
        FROM doctors d 
        JOIN specialties s ON d.specialty_id = s.id 
        WHERE s.name ILIKE $1 || '%'`, shortSpec)
	if err != nil {
		http.Error(w, "DB error", 500)
		return
	}
	defer rows.Close()

	doctors := []Doctor{}
	for rows.Next() {
		var d Doctor
		rows.Scan(&d.ID, &d.Name, &d.Experience)
		doctors = append(doctors, d)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doctors)
}

func getSlotsHandler(w http.ResponseWriter, r *http.Request) {
	doctorID  := r.URL.Query().Get("doctor_id")
	date      := r.URL.Query().Get("date")
	apptType  := r.URL.Query().Get("type")

	table := "appointments"
	if apptType == "online" {
		table = "online_appointments"
	}

	query := fmt.Sprintf(`
        SELECT TO_CHAR(slot_time, 'HH24:MI') 
        FROM %s 
        WHERE doctor_id = $1 AND slot_date = $2`, table)

	rows, err := db.Query(query, doctorID, date)
	if err != nil {
		http.Error(w, "DB error", 500)
		return
	}
	defer rows.Close()

	bookedSlots := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err == nil {
			bookedSlots = append(bookedSlots, strings.TrimSpace(t))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(bookedSlots)
}

func createAppointmentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		return
	}
	var req AppointmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad JSON", 400)
		return
	}

	_, err := db.Exec(`
    INSERT INTO appointments (doctor_id, user_id, slot_date, slot_time, is_online, patient_name, phone, email, doctor_name, specialty)
    VALUES ($1, 0, $2, $3, false, $4, $5, $6, $7, $8)`,
		req.DoctorID, req.Date, req.Time, req.Name, req.Phone, req.Email, req.DoctorName, req.Spec,
	)
	if err != nil {
		log.Printf("Ошибка вставки (offline): %v", err)
		http.Error(w, "DB Error", 500)
		return
	}

	// Сбрасываем кэш администратора при новой записи
	invalidateAdminCache()

	w.WriteHeader(http.StatusCreated)
}

func createOnlineAppointmentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		return
	}
	var req AppointmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad JSON", 400)
		return
	}

	_, err := db.Exec(`
        INSERT INTO online_appointments (patient_name, specialty, doctor_name, appointment_date, slot_date, slot_time, phone, email, doctor_id)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		req.Name, req.Spec, req.DoctorName, req.Date, req.Date, req.Time, req.Phone, req.Email, req.DoctorID,
	)
	if err != nil {
		log.Printf("Ошибка вставки (online): %v", err)
		http.Error(w, "DB Error", 500)
		return
	}

	invalidateAdminCache()

	w.WriteHeader(http.StatusCreated)
}

// Сброс кэша при новой записи
func invalidateAdminCache() {
	if redisClient != nil {
		redisClient.Del(context.Background(), adminCacheKey)
	}
}

// ─── API: Отзывы (MongoDB) ────────────────────────────────────────────────────

func reviewsHandler(w http.ResponseWriter, r *http.Request) {
	if mongoDB == nil {
		http.Error(w, "MongoDB недоступна", 503)
		return
	}
	col := mongoDB.Collection("reviews")
	ctx := context.Background()

	switch r.Method {

	case http.MethodGet:
		// Получаем последние 20 отзывов, сортировка: новые сначала
		opts := options.Find().
			SetSort(bson.D{{Key: "created_at", Value: -1}}).
			SetLimit(20)
		cursor, err := col.Find(ctx, bson.D{}, opts)
		if err != nil {
			http.Error(w, "DB error", 500)
			return
		}
		defer cursor.Close(ctx)
		var reviews []Review
		cursor.All(ctx, &reviews)
		if reviews == nil {
			reviews = []Review{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reviews)

	case http.MethodPost:
		var rev Review
		if err := json.NewDecoder(r.Body).Decode(&rev); err != nil {
			http.Error(w, "Bad JSON", 400)
			return
		}
		// Валидация
		if strings.TrimSpace(rev.Name) == "" || strings.TrimSpace(rev.Text) == "" {
			http.Error(w, "Имя и текст обязательны", 400)
			return
		}
		if rev.Rating < 1 || rev.Rating > 5 {
			http.Error(w, "Рейтинг должен быть от 1 до 5", 400)
			return
		}
		rev.ID = primitive.NewObjectID()
		rev.CreatedAt = time.Now()

		if _, err := col.InsertOne(ctx, rev); err != nil {
			http.Error(w, "Ошибка сохранения", 500)
			return
		}
		w.WriteHeader(http.StatusCreated)

	default:
		http.Error(w, "Method not allowed", 405)
	}
}

// ─── main ─────────────────────────────────────────────────────────────────────

func main() {
	initDB()
	initRedis()
	initMongo()

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// Страницы
	http.HandleFunc("/", homeHandler)
	http.HandleFunc("/booking", bookingHandler)
	http.HandleFunc("/online", onlineHandler)
	http.HandleFunc("/doctors", doctorsPageHandler)
	http.HandleFunc("/admin", adminHandler)

	// API
	http.HandleFunc("/api/get_doctors", getDoctorsHandler)
	http.HandleFunc("/api/get_slots", getSlotsHandler)
	http.HandleFunc("/api/create_appointment", createAppointmentHandler)
	http.HandleFunc("/api/create_online_appointment", createOnlineAppointmentHandler)
	http.HandleFunc("/api/reviews", reviewsHandler)

	fmt.Println("Сервер запущен: http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
