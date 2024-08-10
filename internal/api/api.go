package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/thiagosena/ask-backend/internal/store/pgstore"
)

type apiHandler struct {
	q           *pgstore.Queries
	r           *chi.Mux
	upgrader    websocket.Upgrader
	subscribers map[string]map[*websocket.Conn]context.CancelFunc
	mu          *sync.Mutex
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

func NewHandler(q *pgstore.Queries) http.Handler {
	a := apiHandler{
		q: q,
		upgrader: websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
			return true
		}},
		subscribers: make(map[string]map[*websocket.Conn]context.CancelFunc),
		mu:          &sync.Mutex{},
	}

	r := chi.NewRouter()

	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Get("/subscribe/{room_id}", a.handleSubscribe)

	r.Route("/api", func(r chi.Router) {
		r.Route("/rooms", func(r chi.Router) {
			r.Post("/", a.handleCreateRoom)
			r.Get("/", a.handleGetRoom)
			r.Route("/{room_id}/messages", func(r chi.Router) {
				r.Post("/", a.handleCreateRoomMessages)
				r.Get("/", a.handleGetRoomMessages)

				r.Route("/{message_id}", func(r chi.Router) {
					r.Get("/", a.handleGetRoomMessage)
					r.Patch("/react", a.handleReactToMessage)
					r.Delete("/react", a.handleRemoveReactFromMessage)
					r.Patch("/answer", a.handleMarkMessageAsAnswered)
				})
			})
		})
	})

	a.r = r
	return a
}

const (
	MessageKindMessageCreated = "message_created"
)

type MessageMessageCreated struct {
	Id      string `json:"id"`
	Message string `json:"message"`
}

type Message struct {
	Kind   string `json:"kind"`
	Value  any    `json:"value"`
	RoomId string `json:"-"`
}

func (h apiHandler) notifyCliens(msg Message) {
	h.mu.Lock()
	defer h.mu.Unlock()

	subscribes, ok := h.subscribers[msg.RoomId]
	if !ok || len(subscribes) == 0 {
		return
	}

	for conn, cancel := range subscribes {
		if err := conn.WriteJSON(msg); err != nil {
			slog.Error("Failed to send message to client", "error", err)
			cancel()
		}
	}
}

func (h apiHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	checkRoom, _, rawRoomId := h.checkIfRoomExists(w, r)
	if !checkRoom {
		return
	}

	c, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("Failed to upgrade connection", "error", err)
		http.Error(w, "Failed to upgrate to we connection", http.StatusBadRequest)
		return
	}

	defer c.Close()

	ctx, cancel := context.WithCancel(r.Context())

	h.mu.Lock()
	if _, ok := h.subscribers[rawRoomId]; !ok {
		h.subscribers[rawRoomId] = make(map[*websocket.Conn]context.CancelFunc)
	}
	slog.Info("New client connected", "room_id", rawRoomId, "client_ip", r.RemoteAddr)
	h.subscribers[rawRoomId][c] = cancel

	h.mu.Unlock()
	<-ctx.Done()

	h.mu.Lock()
	delete(h.subscribers[rawRoomId], c)
	h.mu.Unlock()
}

func (h apiHandler) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}
	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	roomId, err := h.q.InsertRoom(r.Context(), body.Theme)
	if err != nil {
		slog.Error("Failed to insert room", "error", err)
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		Id string `json:"id"`
	}

	// TODO: validate errors
	data, _ := json.Marshal(response{Id: roomId.String()})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
func (h apiHandler) handleGetRoom(w http.ResponseWriter, r *http.Request) {
	// Do nothing because of it is not necessary
}
func (h apiHandler) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {
	// Do nothing because of it is not necessary
}

func (h apiHandler) handleCreateRoomMessages(w http.ResponseWriter, r *http.Request) {
	checkRoom, roomId, rawRoomId := h.checkIfRoomExists(w, r)
	if !checkRoom {
		return
	}

	type _body struct {
		Message string `json:"message"`
	}
	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	messageId, err := h.q.InsertMessage(r.Context(), pgstore.InsertMessageParams{RoomID: roomId, Message: body.Message})
	if err != nil {
		slog.Error("Failed to insert message to room", "error", err)
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		Id string `json:"id"`
	}

	// TODO: validate errors
	data, _ := json.Marshal(response{Id: messageId.String()})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)

	go h.notifyCliens(Message{
		Kind:   MessageKindMessageCreated,
		RoomId: rawRoomId,
		Value: MessageMessageCreated{
			Id:      messageId.String(),
			Message: body.Message,
		},
	})

}
func (h apiHandler) handleGetRoomMessage(w http.ResponseWriter, r *http.Request) {
	// Do nothing because of it is not necessary
}
func (h apiHandler) handleReactToMessage(w http.ResponseWriter, r *http.Request) {
	// Do nothing because of it is not necessary
}
func (h apiHandler) handleRemoveReactFromMessage(w http.ResponseWriter, r *http.Request) {
	// Do nothing because of it is not necessary
}
func (h apiHandler) handleMarkMessageAsAnswered(w http.ResponseWriter, r *http.Request) {
	// Do nothing because of it is not necessary
}

func (h apiHandler) checkIfRoomExists(w http.ResponseWriter, r *http.Request) (bool, uuid.UUID, string) {
	rawRoomId := chi.URLParam(r, "room_id")
	roomId, err := uuid.Parse(rawRoomId)
	if err != nil {
		http.Error(w, "Invalid room id", http.StatusBadRequest)
		return false, uuid.Nil, ""
	}

	_, err = h.q.GetRoom(r.Context(), roomId)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Room not found", http.StatusNotFound)
			return false, uuid.Nil, ""
		}

		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return false, uuid.Nil, ""
	}

	return true, roomId, rawRoomId
}
