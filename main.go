package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"

	_ "github.com/lib/pq"
)

var db *sql.DB

type Doctor struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	Experience    string `json:"experience"`
	SpecialtyName string `json:"specialty_name"`
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

func initDB() {
	// Порт 5433 как на твоем скрине
	connStr := "user=postgres password=password dbname=med_db host=localhost port=5433 sslmode=disable"
	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal(err)
	}
	if err = db.Ping(); err != nil {
		log.Fatal("БД недоступна:", err)
	}
	fmt.Println("Связь с базой установлена на порту 5433!")
}

func render(w http.ResponseWriter, tmplName string, data interface{}) {
	tmpl, err := template.ParseFiles(
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

// --- СТРАНИЦЫ ---

func homeHandler(w http.ResponseWriter, r *http.Request) { render(w, "home.html", nil) }
func doctorsPageHandler(w http.ResponseWriter, r *http.Request) {
	rows, _ := db.Query(`SELECT d.id, d.name, d.experience, s.name FROM doctors d JOIN specialties s ON d.specialty_id = s.id`)
	defer rows.Close()
	var doctors []Doctor
	for rows.Next() {
		var d Doctor
		rows.Scan(&d.ID, &d.Name, &d.Experience, &d.SpecialtyName)
		doctors = append(doctors, d)
	}
	render(w, "doctors.html", doctors)
}

func bookingHandler(w http.ResponseWriter, r *http.Request) {
	spec := r.URL.Query().Get("spec")
	render(w, "booking.html", map[string]string{"SelectedSpec": spec})
}

func onlineHandler(w http.ResponseWriter, r *http.Request) {
	spec := r.URL.Query().Get("spec")
	render(w, "online.html", map[string]string{"SelectedSpec": spec})
}

// --- API ---

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
		log.Printf("Ошибка поиска врачей: %v", err)
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

// Находим функцию getSlotsHandler и заменяем запрос:
func getSlotsHandler(w http.ResponseWriter, r *http.Request) {
	doctorID := r.URL.Query().Get("doctor_id")
	date := r.URL.Query().Get("date")
	apptType := r.URL.Query().Get("type")

	table := "appointments"
	if apptType == "online" {
		table = "online_appointments"
	}

	// ИСПОЛЬЗУЕМ TO_CHAR для удаления секунд прямо в SQL
	query := fmt.Sprintf(`
        SELECT TO_CHAR(slot_time, 'HH24:MI') 
        FROM %s 
        WHERE doctor_id = $1 AND slot_date = $2`, table)

	rows, err := db.Query(query, doctorID, date)
	if err != nil {
		log.Printf("Ошибка получения слотов: %v", err)
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
    INSERT INTO appointments (
        doctor_id, user_id, slot_date, slot_time, is_online, 
        patient_name, phone, email, doctor_name, specialty  
    )
    VALUES ($1, 0, $2, $3, false, $4, $5, $6, $7, $8)`,
		req.DoctorID, req.Date, req.Time, req.Name, req.Phone, req.Email, req.DoctorName, req.Spec,
	)
	if err != nil {
		log.Printf("Ошибка вставки (offline): %v", err)
		http.Error(w, "DB Error", 500)
		return
	}
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

	// Заполняем ВСЕ колонки дат и времени, чтобы не было NULL
	query := `
        INSERT INTO online_appointments (
            patient_name, specialty, doctor_name, 
            appointment_date, slot_date, slot_time, 
            phone, email, doctor_id
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	_, err := db.Exec(query,
		req.Name, req.Spec, req.DoctorName,
		req.Date, // в appointment_date
		req.Date, // в slot_date
		req.Time, // в slot_time
		req.Phone, req.Email, req.DoctorID,
	)

	if err != nil {
		log.Printf("Ошибка вставки (online): %v", err)
		http.Error(w, "DB Error", 500)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func main() {
	initDB()

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	http.HandleFunc("/", homeHandler)
	http.HandleFunc("/booking", bookingHandler)
	http.HandleFunc("/online", onlineHandler)
	http.HandleFunc("/doctors", doctorsPageHandler)

	http.HandleFunc("/api/get_doctors", getDoctorsHandler)
	http.HandleFunc("/api/get_slots", getSlotsHandler)
	http.HandleFunc("/api/create_appointment", createAppointmentHandler)
	http.HandleFunc("/api/create_online_appointment", createOnlineAppointmentHandler)

	fmt.Println("Сервер запущен: http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
