package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/template/html/v2"
	"github.com/gofiber/websocket/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

//go:embed frontend/templates/*.html
var templates embed.FS

//go:embed frontend/static/*
var static embed.FS

// estrutura user
type User struct {
	ID        string    `json:"id" db:"id"`
	Username  string    `json:"username" db:"username"`
	Email     string    `json:"email" db:"email"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	Avatar    string    `json:"avatar" db:"avatar"`
	Role      string    `json:"role" db:"role"`
}

// estrutura board
type Board struct {
	ID          int       `json:"id" db:"id"`
	Title       string    `json:"title" db:"title"`
	Description string    `json:"description" db:"description"`
	OwnerID     string    `json:"owner_id" db:"owner_id"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" db:"updated_at"`
	Color       string    `json:"color" db:"color"`
	IsPublic    bool      `json:"is_public" db:"is_public"`
	OwnerName   string    `json:"owner_name,omitempty" db:"owner_name"`
}

// estrutura column
type Column struct {
	ID       int    `json:"id" db:"id"`
	BoardID  int    `json:"board_id" db:"board_id"`
	Title    string `json:"title" db:"title"`
	Position int    `json:"position" db:"position"`
	Color    string `json:"color" db:"color"`
}

// estrutura card
type Card struct {
	ID          int        `json:"id" db:"id"`
	ColumnID    int        `json:"column_id" db:"column_id"`
	Title       string     `json:"title" db:"title"`
	Description string     `json:"description" db:"description"`
	AssignedTo  string     `json:"assigned_to" db:"assigned_to"`
	Priority    string     `json:"priority" db:"priority"`
	DueDate     *time.Time `json:"due_date" db:"due_date"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at" db:"updated_at"`
	Position    int        `json:"position" db:"position"`
}

// estrutura notification
type Notification struct {
	ID             int       `json:"id"`
	UserID         string    `json:"user_id"`
	Type           string    `json:"type"`
	Message        string    `json:"message"`
	IsRead         bool      `json:"is_read"`
	RelatedBoardID *int      `json:"related_board_id,omitempty"`
	RelatedCardID  *int      `json:"related_card_id,omitempty"`
	InvitationID   *int      `json:"invitation_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// estrutura boardinvitation
type BoardInvitation struct {
	ID          int       `json:"id" db:"id"`
	BoardID     int       `json:"board_id" db:"board_id"`
	BoardTitle  string    `json:"board_title" db:"board_title"`
	InviterID   string    `json:"inviter_id" db:"inviter_id"`
	InviterName string    `json:"inviter_name" db:"inviter_name"`
	Status      string    `json:"status" db:"status"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
}

// estrutura wsmessage
type WsMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

// estrutura reorderpayload
type ReorderPayload struct {
	ColumnID       int   `json:"column_id"`
	OrderedCardIDs []int `json:"ordered_card_ids"`
}

// estrutura App
type App struct {
	db       *pgxpool.Pool
	clients  map[int]map[*websocket.Conn]bool
	colLocks struct {
		mu    sync.Mutex
		locks map[int]*sync.Mutex
	}
}

// mapeamento global
var userDisplayNameMap = map[string]string{
	"eduardo@kanban.local": "Eduardo Tomaz",
	"alison@kanban.local":  "Alison Silva",
	"marques@kanban.local": "Gabriel Marques",
	"rosa@kanban.local":    "Gabriel Rosa",
	"miyake@kanban.local":  "João Miyake",
	"gomes@kanban.local":   "João Gomes",
	"rodrigo@kanban.local": "Rodrigo Akira",
	"rubens@kanban.local":  "Rubens Leite",
	"kaiky@kanban.local":   "Kaiky Leandro",
	"pedro@kanban.local":   "Pedro Santos",
}

// pegar mapeamento
func (app *App) getDisplayName(ctx context.Context, tx pgx.Tx, userID string) string {
	var email string
	var username sql.NullString

	var querier interface {
		QueryRow(context.Context, string, ...interface{}) pgx.Row
	}
	if tx != nil {
		querier = tx
	} else {
		querier = app.db
	}

	err := querier.QueryRow(ctx, "SELECT email, raw_user_meta_data->>'username' FROM auth.users WHERE id = $1", userID).Scan(&email, &username)
	if err != nil {
		log.Printf("Aviso: não foi possível encontrar o nome para o userID %s: %v", userID, err)
		return "Um usuário"
	}

	if name, ok := userDisplayNameMap[email]; ok {
		return name
	}
	if username.Valid && username.String != "" {
		return username.String
	}
	return email
}

// mutex
func (app *App) getColumnLock(columnID int) *sync.Mutex {
	app.colLocks.mu.Lock()
	defer app.colLocks.mu.Unlock()

	lock, ok := app.colLocks.locks[columnID]
	if !ok {
		lock = &sync.Mutex{}
		app.colLocks.locks[columnID] = lock
	}
	return lock
}

// claims Supabase JWT
type SupabaseClaims struct {
	UserID string `json:"sub"`
	jwt.RegisteredClaims
}

// conexão com database
func (app *App) connectDB() error {
	config, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("não foi possível parsear a URL do banco: %w", err)
	}
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return fmt.Errorf("não foi possível conectar ao banco de dados: %w", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		return fmt.Errorf("não foi possível pingar o banco de dados: %w", err)
	}
	app.db = pool
	return nil
}

// middleware auth
func (app *App) authMiddleware(c *fiber.Ctx) error {
	authHeader := c.Get("Authorization")
	if authHeader == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Cabeçalho de autorização ausente"})
	}
	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Formato de autorização inválido. Esperado: Bearer <token>"})
	}
	tokenString := parts[1]
	jwtSecret := os.Getenv("SUPABASE_JWT_SECRET")
	if jwtSecret == "" {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Configuração do servidor incorreta"})
	}
	token, err := jwt.ParseWithClaims(tokenString, &SupabaseClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("método de assinatura inesperado: %v", token.Header["alg"])
		}
		return []byte(jwtSecret), nil
	}, jwt.WithAudience("authenticated"))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Token expirado"})
		}
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Token inválido"})
	}
	if !token.Valid {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Token inválido ou expirado"})
	}
	claims, ok := token.Claims.(*SupabaseClaims)
	if !ok || claims.UserID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Claims do token inválidas ou ID de usuário ausente"})
	}
	c.Locals("userID", claims.UserID)
	return c.Next()
}

// websocket
func (app *App) handleWebSocket(c *websocket.Conn) {
	boardID, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		c.Close()
		return
	}
	if app.clients[boardID] == nil {
		app.clients[boardID] = make(map[*websocket.Conn]bool)
	}
	app.clients[boardID][c] = true
	defer func() {
		delete(app.clients[boardID], c)
		if len(app.clients[boardID]) == 0 {
			delete(app.clients, boardID)
		}
		c.Close()
	}()
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			break
		}
	}
}

// broadcast
func (app *App) broadcast(boardID int, message WsMessage) {
	if clients, ok := app.clients[boardID]; ok {
		payloadBytes, _ := json.Marshal(message)
		for client := range clients {
			if err := client.WriteMessage(websocket.TextMessage, payloadBytes); err != nil {
				client.Close()
				delete(clients, client)
			}
		}
	}
}

// avatar users
func (app *App) handleAvatarUpload(c *fiber.Ctx) error {
	userID := c.Locals("userID").(string)
	file, err := c.FormFile("avatar")
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Nenhum arquivo de avatar enviado"})
	}
	contentType := file.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Formato de arquivo inválido. Apenas imagens são permitidas."})
	}
	src, err := file.Open()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Erro ao abrir o arquivo"})
	}
	defer src.Close()
	fileBytes, err := io.ReadAll(src)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Erro ao ler o arquivo"})
	}
	ext := filepath.Ext(file.Filename)
	fileName := fmt.Sprintf("avatar-%s%s", userID, ext)
	supabaseURL := os.Getenv("SUPABASE_PROJECT_URL")
	supabaseKey := os.Getenv("SUPABASE_SERVICE_KEY")
	uploadURL := fmt.Sprintf("%s/storage/v1/object/avatars/%s", supabaseURL, fileName)
	req, err := http.NewRequest("POST", uploadURL, bytes.NewReader(fileBytes))
	if err != nil {
		log.Printf("❌ Erro ao criar requisição para o Supabase: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Erro interno ao preparar upload"})
	}
	req.Header.Set("Authorization", "Bearer "+supabaseKey)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-upsert", "true")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("❌ Erro ao fazer upload para o Supabase: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Erro interno ao fazer upload"})
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("❌ Supabase retornou status não-OK: %s, Body: %s", resp.Status, string(body))
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Falha ao armazenar o arquivo"})
	}
	publicURL := fmt.Sprintf("%s/storage/v1/object/public/avatars/%s", supabaseURL, fileName)
	query := `
		UPDATE auth.users
		SET raw_user_meta_data = raw_user_meta_data || jsonb_build_object('avatar_url', $1::text)
		WHERE id = $2
	`
	_, err = app.db.Exec(context.Background(), query, publicURL, userID)
	if err != nil {
		log.Printf("❌ Erro ao atualizar o avatar do usuário no DB: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Erro ao atualizar perfil"})
	}
	return c.JSON(fiber.Map{"avatar_url": publicURL})
}

// board por id de card
func (app *App) getBoardIDFromCard(cardID int) (int, error) {
	var boardID int
	query := `SELECT c.board_id FROM columns c
			  INNER JOIN cards ca ON c.id = ca.column_id
			  WHERE ca.id = $1`
	err := app.db.QueryRow(context.Background(), query, cardID).Scan(&boardID)
	if err != nil {
		return 0, err
	}
	return boardID, nil
}

// endpoint criar coluna
func (app *App) createColumn(c *fiber.Ctx) error {
	var col Column
	if err := c.BodyParser(&col); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "dados de coluna inválidos"})
	}
	if col.BoardID == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "board_id é obrigatório"})
	}
	var maxPos sql.NullInt64
	err := app.db.QueryRow(context.Background(),
		"SELECT MAX(position) FROM columns WHERE board_id = $1", col.BoardID).Scan(&maxPos)
	if err != nil {
		maxPos.Int64 = -1
	}
	col.Position = int(maxPos.Int64) + 1
	query := `
        INSERT INTO columns (board_id, title, position, color)
        VALUES ($1, $2, $3, $4)
        RETURNING id
    `
	err = app.db.QueryRow(context.Background(), query,
		col.BoardID, col.Title, col.Position, col.Color).Scan(&col.ID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "erro ao criar coluna"})
	}
	app.broadcast(col.BoardID, WsMessage{Type: "COLUMN_CREATED", Payload: col})
	return c.Status(201).JSON(col)
}

// endpoint deletar coluna
func (app *App) deleteColumn(c *fiber.Ctx) error {
	columnID, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "ID da coluna inválido"})
	}
	tx, err := app.db.Begin(context.Background())
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro interno do servidor"})
	}
	defer tx.Rollback(context.Background())
	var cardCount int
	err = tx.QueryRow(context.Background(), "SELECT COUNT(*) FROM cards WHERE column_id = $1", columnID).Scan(&cardCount)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao verificar cards na coluna"})
	}
	if cardCount > 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "A coluna não pode ser excluída pois contém tarefas."})
	}
	var boardID, position int
	err = tx.QueryRow(context.Background(), "SELECT board_id, position FROM columns WHERE id = $1", columnID).Scan(&boardID, &position)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "Coluna não encontrada"})
	}
	_, err = tx.Exec(context.Background(), "DELETE FROM columns WHERE id = $1", columnID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao deletar a coluna"})
	}
	_, err = tx.Exec(context.Background(), "UPDATE columns SET position = position - 1 WHERE board_id = $1 AND position > $2", boardID, position)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao reordenar colunas"})
	}
	if err := tx.Commit(context.Background()); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao confirmar a exclusão"})
	}
	app.broadcast(boardID, WsMessage{Type: "BOARD_STATE_UPDATED", Payload: nil})
	return c.Status(200).JSON(fiber.Map{"status": "deleted"})
}

// endpoint deletar board
func (app *App) deleteBoard(c *fiber.Ctx) error {
	boardID, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "ID de board inválido"})
	}
	userID := c.Locals("userID").(string)
	var ownerID string
	err = app.db.QueryRow(context.Background(), "SELECT owner_id FROM boards WHERE id = $1", boardID).Scan(&ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Quadro não encontrado"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Erro ao verificar o quadro"})
	}
	if ownerID != userID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Acesso negado. Você não é o dono deste quadro."})
	}
	_, err = app.db.Exec(context.Background(), "DELETE FROM boards WHERE id = $1", boardID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Erro ao deletar o quadro"})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// endpoints rotas API
func (app *App) setupRoutes(fiberApp *fiber.App) {
	api := fiberApp.Group("/api")
	protected := api.Use(app.authMiddleware)

	protected.Get("/users", app.getUsers)
	protected.Post("/user/avatar", app.handleAvatarUpload)
	protected.Get("/boards/public", app.getPublicBoards)
	protected.Get("/boards/private", app.getPrivateBoards)
	protected.Post("/boards", app.createBoard)
	protected.Delete("/boards/:id", app.deleteBoard)
	protected.Get("/boards/:id/columns", app.getColumns)
	protected.Post("/columns", app.createColumn)
	protected.Delete("/columns/:id", app.deleteColumn)
	protected.Get("/columns/:id/cards", app.getCards)
	protected.Post("/columns/:id/cards", app.createCard)
	protected.Put("/cards/:id", app.updateCard)
	protected.Delete("/cards/:id", app.deleteCard)
	protected.Post("/cards/reorder", app.reorderCards)

	// Rotas de Membros e Convites
	protected.Get("/boards/:id/members", app.getBoardMembers)
	protected.Get("/boards/:id/invitable-users", app.getInvitableUsers)
	protected.Post("/boards/:id/invite", app.inviteUserToBoard)
	protected.Post("/invitations/:id/respond", app.respondToInvitation)

	// Rota para remover membros
	protected.Delete("/boards/:boardId/members/:memberId", app.removeBoardMember)
	protected.Post("/boards/:id/leave", app.leaveBoard)

	// Rotas de Notificação
	protected.Get("/notifications", app.getNotifications)
	protected.Post("/notifications/:id/read", app.markNotificationRead)
	protected.Post("/notifications/mark-all-as-read", app.markAllNotificationsRead)

}

// endpoint users
func (app *App) getUsers(c *fiber.Ctx) error {
	conn, err := app.db.Acquire(context.Background())
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "erro de conexão"})
	}
	defer conn.Release()
	users := make([]User, 0)
	query := `
        SELECT
            id,
            email,
            COALESCE(raw_user_meta_data->>'username', email) as username,
            COALESCE(raw_user_meta_data->>'avatar_url', '') as avatar,
            created_at,
            COALESCE(role, '') as role
        FROM auth.users ORDER BY email
    `
	rows, err := conn.Query(context.Background(), query)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "erro ao buscar usuários"})
	}
	defer rows.Close()
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.Email, &user.Username, &user.Avatar, &user.CreatedAt, &user.Role); err != nil {
			continue
		}
		users = append(users, user)
	}
	return c.JSON(users)
}

// endpoint boards publicos
func (app *App) getPublicBoards(c *fiber.Ctx) error {
	query := `SELECT id, title, description, owner_id, created_at, updated_at, color, is_public
			  FROM boards
			  WHERE is_public = true
			  ORDER BY created_at DESC LIMIT 1`
	rows, err := app.db.Query(context.Background(), query)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "erro ao buscar boards públicos"})
	}
	defer rows.Close()
	boards := make([]Board, 0)
	for rows.Next() {
		var board Board
		if err := rows.Scan(&board.ID, &board.Title, &board.Description,
			&board.OwnerID, &board.CreatedAt, &board.UpdatedAt,
			&board.Color, &board.IsPublic); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "erro ao ler board"})
		}
		boards = append(boards, board)
	}
	return c.JSON(boards)
}

// endpoint boards privados
func (app *App) getPrivateBoards(c *fiber.Ctx) error {
	userID := c.Locals("userID").(string)

	ownerQuery := `SELECT id, title, description, owner_id, created_at, updated_at, color, is_public
                   FROM boards
                   WHERE owner_id = $1 AND is_public = false`

	memberQuery := `SELECT b.id, b.title, b.description, b.owner_id, b.created_at, b.updated_at, b.color, b.is_public,
                           COALESCE(u.raw_user_meta_data->>'username', u.email) as owner_name
                    FROM boards b
                    JOIN board_memberships bm ON b.id = bm.board_id
                    JOIN auth.users u ON b.owner_id = u.id
                    WHERE bm.user_id = $1 AND b.owner_id != $1`

	boards := make([]Board, 0)
	boardIDs := make(map[int]bool)

	rows, err := app.db.Query(context.Background(), ownerQuery, userID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "erro ao buscar seus boards privados"})
	}
	defer rows.Close()

	for rows.Next() {
		var board Board
		if err := rows.Scan(&board.ID, &board.Title, &board.Description, &board.OwnerID, &board.CreatedAt, &board.UpdatedAt, &board.Color, &board.IsPublic); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "erro ao ler board privado"})
		}
		if !boardIDs[board.ID] {
			boards = append(boards, board)
			boardIDs[board.ID] = true
		}
	}
	rows.Close()

	rows, err = app.db.Query(context.Background(), memberQuery, userID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "erro ao buscar boards compartilhados"})
	}
	defer rows.Close()

	for rows.Next() {
		var board Board
		if err := rows.Scan(&board.ID, &board.Title, &board.Description, &board.OwnerID, &board.CreatedAt, &board.UpdatedAt, &board.Color, &board.IsPublic, &board.OwnerName); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "erro ao ler board compartilhado"})
		}
		if !boardIDs[board.ID] {
			boards = append(boards, board)
			boardIDs[board.ID] = true
		}
	}

	return c.JSON(boards)
}

// endpoint criar board
func (app *App) createBoard(c *fiber.Ctx) error {
	var reqBoard Board
	if err := c.BodyParser(&reqBoard); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "dados inválidos"})
	}
	userID := c.Locals("userID").(string)
	if reqBoard.Title == "" {
		return c.Status(400).JSON(fiber.Map{"error": "O título do quadro é obrigatório"})
	}
	tx, err := app.db.Begin(context.Background())
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "erro ao iniciar transação"})
	}
	defer tx.Rollback(context.Background())
	query := `INSERT INTO boards (title, description, owner_id, is_public, color)
			  VALUES ($1, $2, $3, $4, $5)
			  RETURNING id, created_at, updated_at`
	err = tx.QueryRow(context.Background(), query,
		reqBoard.Title, reqBoard.Description, userID, reqBoard.IsPublic, reqBoard.Color).Scan(&reqBoard.ID, &reqBoard.CreatedAt, &reqBoard.UpdatedAt)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "erro ao criar board"})
	}
	reqBoard.OwnerID = userID
	defaultColumns := []struct {
		Title string
		Color string
	}{
		{"A Fazer", "#58a6ff"},
		{"Em Andamento", "#d29922"},
		{"Concluído", "#3fb950"},
	}
	colQuery := `INSERT INTO columns (board_id, title, position, color) VALUES ($1, $2, $3, $4)`
	for i, col := range defaultColumns {
		if _, err := tx.Exec(context.Background(), colQuery, reqBoard.ID, col.Title, i, col.Color); err != nil {
			log.Printf("Erro ao criar coluna padrão '%s' para o board %d: %v", col.Title, reqBoard.ID, err)
			return c.Status(500).JSON(fiber.Map{"error": "erro ao criar colunas padrão"})
		}
	}
	if err := tx.Commit(context.Background()); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "erro ao confirmar criação do board"})
	}
	return c.Status(201).JSON(reqBoard)
}

// endpoint colunas
func (app *App) getColumns(c *fiber.Ctx) error {
	boardID, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "ID de board inválido"})
	}
	userID := c.Locals("userID").(string)
	hasPermission, err := app.checkBoardPermission(userID, boardID)
	if err != nil || !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Acesso negado a este quadro."})
	}
	var isPublic bool
	app.db.QueryRow(context.Background(), "SELECT is_public FROM boards WHERE id = $1", boardID).Scan(&isPublic)

	if isPublic {
		return app.getPublicBoardColumns(c, boardID)
	}

	query := `SELECT id, board_id, title, position, COALESCE(color, '#e4e6ea') as color
			  FROM columns WHERE board_id = $1 ORDER BY position`
	rows, err := app.db.Query(context.Background(), query, boardID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "erro ao buscar colunas"})
	}
	defer rows.Close()

	columns := make([]Column, 0)
	for rows.Next() {
		var col Column
		if err := rows.Scan(&col.ID, &col.BoardID, &col.Title, &col.Position, &col.Color); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "erro ao ler dados da coluna"})
		}
		columns = append(columns, col)
	}
	return c.JSON(columns)
}

// pegar boards publicos
func (app *App) getPublicBoardColumns(c *fiber.Ctx, boardID int) error {
	tx, err := app.db.Begin(context.Background())
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao iniciar transação de verificação"})
	}
	defer tx.Rollback(context.Background())
	query := `SELECT id, board_id, title, position, COALESCE(color, '#e4e6ea') as color
			  FROM columns WHERE board_id = $1 ORDER BY position`
	rows, err := tx.Query(context.Background(), query, boardID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "erro ao buscar colunas"})
	}
	columns := make([]Column, 0)
	foundSolucionado := false
	foundNaoSolucionado := false
	maxPosition := -1
	for rows.Next() {
		var col Column
		if err := rows.Scan(&col.ID, &col.BoardID, &col.Title, &col.Position, &col.Color); err != nil {
			rows.Close()
			return c.Status(500).JSON(fiber.Map{"error": "erro ao ler dados da coluna"})
		}
		titleLower := strings.ToLower(col.Title)
		if titleLower == "solucionado" {
			foundSolucionado = true
		}
		if titleLower == "não solucionado" {
			foundNaoSolucionado = true
		}
		if col.Position > maxPosition {
			maxPosition = col.Position
		}
		columns = append(columns, col)
	}
	rows.Close()
	if !foundSolucionado {
		maxPosition++
		_, err = tx.Exec(context.Background(),
			`INSERT INTO columns (board_id, title, position, color) VALUES ($1, $2, $3, $4)`,
			boardID, "Solucionado", maxPosition, "#3fb950")
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "erro ao criar coluna 'Solucionado'"})
		}
	}
	if !foundNaoSolucionado {
		maxPosition++
		_, err = tx.Exec(context.Background(),
			`INSERT INTO columns (board_id, title, position, color) VALUES ($1, $2, $3, $4)`,
			boardID, "Não Solucionado", maxPosition, "#f85149")
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "erro ao criar coluna 'Não Solucionado'"})
		}
	}
	if err := tx.Commit(context.Background()); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao confirmar criação de colunas de status"})
	}
	if !foundSolucionado || !foundNaoSolucionado {
		rows, err = app.db.Query(context.Background(), query, boardID)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "erro ao re-buscar colunas após correção"})
		}
		defer rows.Close()
		columns = make([]Column, 0)
		for rows.Next() {
			var col Column
			if err := rows.Scan(&col.ID, &col.BoardID, &col.Title, &col.Position, &col.Color); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": "erro ao ler dados da coluna após correção"})
			}
			columns = append(columns, col)
		}
	}
	return c.JSON(columns)
}

// endpoint cards
func (app *App) getCards(c *fiber.Ctx) error {
	columnID, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "ID de coluna inválido"})
	}
	userID := c.Locals("userID").(string)
	boardID, err := app.getBoardIDFromColumn(columnID)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "Coluna não encontrada."})
	}
	hasPermission, err := app.checkBoardPermission(userID, boardID)
	if err != nil || !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Acesso negado a este quadro."})
	}
	rows, err := app.db.Query(context.Background(), `
		SELECT id, column_id, title, COALESCE(description, '') as description,
			   COALESCE(assigned_to, '') as assigned_to, COALESCE(priority, 'media') as priority,
			   due_date, position, created_at, updated_at
		FROM cards WHERE column_id = $1 ORDER BY position`, columnID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "erro ao buscar cards"})
	}
	defer rows.Close()
	cards := make([]Card, 0)
	for rows.Next() {
		var card Card
		if err := rows.Scan(&card.ID, &card.ColumnID, &card.Title, &card.Description,
			&card.AssignedTo, &card.Priority, &card.DueDate, &card.Position,
			&card.CreatedAt, &card.UpdatedAt); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "erro ao ler dados do card"})
		}
		cards = append(cards, card)
	}
	return c.JSON(cards)
}

// pegar user por id auth
func (app *App) getUserIDByUsername(username string) (string, error) {
	var userID string
	query := `SELECT id FROM auth.users WHERE raw_user_meta_data->>'username' = $1 OR email = $1 LIMIT 1`
	err := app.db.QueryRow(context.Background(), query, username).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("usuário '%s' não encontrado", username)
		}
		return "", err
	}
	return userID, nil
}

// endpoint notificacao
func (app *App) createNotification(tx pgx.Tx, n Notification) error {
	query := `INSERT INTO notifications 
              (user_id, type, message, related_board_id, related_card_id, invitation_id) 
              VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := tx.Exec(context.Background(), query,
		n.UserID, n.Type, n.Message, n.RelatedBoardID, n.RelatedCardID, n.InvitationID)
	return err
}

func (app *App) createCard(c *fiber.Ctx) error {
	columnID, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "ID da coluna inválido"})
	}
	userID := c.Locals("userID").(string)
	boardID, err := app.getBoardIDFromColumn(columnID)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "Coluna não encontrada"})
	}
	hasPermission, err := app.checkBoardPermission(userID, boardID)
	if err != nil || !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Acesso negado"})
	}
	var card Card
	if err := c.BodyParser(&card); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Dados de card inválidos"})
	}
	card.ColumnID = columnID
	tx, err := app.db.Begin(context.Background())
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao iniciar transação"})
	}
	defer tx.Rollback(context.Background())
	var maxPos sql.NullInt64
	tx.QueryRow(context.Background(), "SELECT MAX(position) FROM cards WHERE column_id = $1", columnID).Scan(&maxPos)
	card.Position = int(maxPos.Int64) + 1
	query := `INSERT INTO cards (column_id, title, description, assigned_to, priority, due_date, position) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at, updated_at`
	err = tx.QueryRow(context.Background(), query, card.ColumnID, card.Title, card.Description, card.AssignedTo, card.Priority, card.DueDate, card.Position).Scan(&card.ID, &card.CreatedAt, &card.UpdatedAt)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao criar card"})
	}
	if card.AssignedTo != "" {
		assigneeID, err := app.getUserIDByUsername(card.AssignedTo)
		if err == nil {
			notification := Notification{
				UserID:         assigneeID,
				Type:           "new_task_assigned",
				Message:        fmt.Sprintf("Você foi atribuído à tarefa: %s", card.Title),
				RelatedBoardID: &boardID,
				RelatedCardID:  &card.ID,
			}
			app.createNotification(tx, notification)
		}
	}
	if err := tx.Commit(context.Background()); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao confirmar criação"})
	}
	app.broadcast(boardID, WsMessage{Type: "CARD_CREATED", Payload: card})
	return c.Status(201).JSON(card)
}

// endpoint att card
func (app *App) updateCard(c *fiber.Ctx) error {
	cardID, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "ID do card inválido"})
	}
	var newCardData Card
	if err := c.BodyParser(&newCardData); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Dados de card inválidos"})
	}
	tx, err := app.db.Begin(context.Background())
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao iniciar transação"})
	}
	defer tx.Rollback(context.Background())
	var oldCardData Card
	err = tx.QueryRow(context.Background(), "SELECT assigned_to FROM cards WHERE id = $1", cardID).Scan(&oldCardData.AssignedTo)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "Tarefa original não encontrada"})
	}
	query := `UPDATE cards SET title = $1, description = $2, assigned_to = $3, priority = $4, due_date = $5, updated_at = NOW() WHERE id = $6`
	_, err = tx.Exec(context.Background(), query, newCardData.Title, newCardData.Description, newCardData.AssignedTo, newCardData.Priority, newCardData.DueDate, cardID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao atualizar card"})
	}
	if newCardData.AssignedTo != "" && newCardData.AssignedTo != oldCardData.AssignedTo {
		boardID, err := app.getBoardIDFromCard(cardID)
		if err == nil {
			assigneeID, err := app.getUserIDByUsername(newCardData.AssignedTo)
			if err == nil {
				notification := Notification{
					UserID:         assigneeID,
					Type:           "new_task_assigned",
					Message:        fmt.Sprintf("Você foi atribuído à tarefa: %s", newCardData.Title),
					RelatedBoardID: &boardID,
					RelatedCardID:  &cardID,
				}
				app.createNotification(tx, notification)
			}
		}
	}
	if err := tx.Commit(context.Background()); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao confirmar atualização"})
	}
	boardID, err := app.getBoardIDFromCard(cardID)
	if err == nil {
		var updatedCard Card
		selectQuery := `SELECT id, column_id, title, COALESCE(description, '') as description, COALESCE(assigned_to, '') as assigned_to, COALESCE(priority, 'media') as priority, due_date, position, created_at, updated_at FROM cards WHERE id = $1`
		app.db.QueryRow(context.Background(), selectQuery, cardID).Scan(&updatedCard.ID, &updatedCard.ColumnID, &updatedCard.Title, &updatedCard.Description, &updatedCard.AssignedTo, &updatedCard.Priority, &updatedCard.DueDate, &updatedCard.Position, &updatedCard.CreatedAt, &updatedCard.UpdatedAt)
		app.broadcast(boardID, WsMessage{Type: "CARD_UPDATED", Payload: updatedCard})
	}
	return c.Status(200).JSON(fiber.Map{"status": "updated"})
}

// permissao dos boards
func (app *App) checkBoardPermission(userID string, boardID int) (bool, error) {
	var isPublic bool
	err := app.db.QueryRow(context.Background(), "SELECT is_public FROM boards WHERE id = $1", boardID).Scan(&isPublic)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if isPublic {
		return true, nil
	}

	var ownerID string
	err = app.db.QueryRow(context.Background(), "SELECT owner_id FROM boards WHERE id = $1", boardID).Scan(&ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if ownerID == userID {
		return true, nil
	}

	var memberCount int
	err = app.db.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM board_memberships WHERE board_id = $1 AND user_id = $2",
		boardID, userID).Scan(&memberCount)
	if err != nil {
		return false, err
	}
	if memberCount > 0 {
		return true, nil
	}
	return false, nil
}

// pegar id do board por coluna
func (app *App) getBoardIDFromColumn(columnID int) (int, error) {
	var boardID int
	query := `SELECT board_id FROM columns WHERE id = $1`
	err := app.db.QueryRow(context.Background(), query, columnID).Scan(&boardID)
	if err != nil {
		return 0, err
	}
	return boardID, nil
}

// endpoint deletar card
func (app *App) deleteCard(c *fiber.Ctx) error {
	cardID, _ := strconv.Atoi(c.Params("id"))
	boardID, err := app.getBoardIDFromCard(cardID)
	if err != nil {
	}
	_, err = app.db.Exec(context.Background(), `DELETE FROM cards WHERE id = $1`, cardID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "erro ao deletar card"})
	}
	if boardID != 0 {
		app.broadcast(boardID, WsMessage{Type: "CARD_DELETED", Payload: fiber.Map{"card_id": cardID}})
	}
	return c.Status(200).JSON(fiber.Map{"status": "deleted"})
}

// endpoint reordenar card
func (app *App) reorderCards(c *fiber.Ctx) error {
	var payload ReorderPayload
	if err := c.BodyParser(&payload); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Payload inválido"})
	}

	columnLock := app.getColumnLock(payload.ColumnID)
	columnLock.Lock()
	defer columnLock.Unlock()

	tx, err := app.db.Begin(context.Background())
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao iniciar transação"})
	}
	defer tx.Rollback(context.Background())

	stmt, err := tx.Prepare(context.Background(), fmt.Sprintf("update_card_order_col_%d", payload.ColumnID),
		"UPDATE cards SET position = $1, column_id = $2, updated_at = NOW() WHERE id = $3")
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao preparar a query"})
	}

	for i, cardID := range payload.OrderedCardIDs {
		if _, err := tx.Exec(context.Background(), stmt.Name, i, payload.ColumnID, cardID); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": fmt.Sprintf("Erro ao atualizar card ID %d", cardID)})
		}
	}

	if err := tx.Commit(context.Background()); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao confirmar a reordenação"})
	}

	go func() {
		boardID, boardIDErr := app.getBoardIDFromColumn(payload.ColumnID)
		if boardIDErr == nil && boardID != 0 {
			app.broadcast(boardID, WsMessage{Type: "BOARD_STATE_UPDATED", Payload: nil})
		}
	}()

	return c.Status(200).JSON(fiber.Map{"status": "reordered"})
}

// endpoint sair do board
func (app *App) leaveBoard(c *fiber.Ctx) error {
	boardID, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "ID do quadro inválido"})
	}
	userID := c.Locals("userID").(string)

	var ownerID string
	err = app.db.QueryRow(context.Background(), "SELECT owner_id FROM boards WHERE id = $1", boardID).Scan(&ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Quadro não encontrado"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Erro ao verificar o quadro"})
	}

	if ownerID == userID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "O dono do quadro não pode sair. Use a opção de excluir o quadro."})
	}

	_, err = app.db.Exec(context.Background(), "DELETE FROM board_memberships WHERE board_id = $1 AND user_id = $2", boardID, userID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Falha ao sair do quadro."})
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// pegar users convidados
func (app *App) getInvitableUsers(c *fiber.Ctx) error {
	boardID, _ := strconv.Atoi(c.Params("id"))
	currentUserID := c.Locals("userID").(string)

	allUsers := make([]User, 0)
	queryAll := `SELECT id, email, COALESCE(raw_user_meta_data->>'username', email) as username, COALESCE(raw_user_meta_data->>'avatar_url', '') as avatar FROM auth.users WHERE id != $1`
	rows, err := app.db.Query(context.Background(), queryAll, currentUserID)
	if err != nil {
		log.Printf("Erro ao buscar todos os usuários: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao buscar usuários"})
	}
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.Email, &user.Username, &user.Avatar); err == nil {
			allUsers = append(allUsers, user)
		}
	}
	rows.Close()

	exclusionIDs := make(map[string]bool)
	var ownerID string
	app.db.QueryRow(context.Background(), "SELECT owner_id FROM boards WHERE id = $1", boardID).Scan(&ownerID)
	if ownerID != "" {
		exclusionIDs[ownerID] = true
	}

	memberRows, _ := app.db.Query(context.Background(), "SELECT user_id FROM board_memberships WHERE board_id = $1", boardID)
	for memberRows.Next() {
		var id string
		memberRows.Scan(&id)
		exclusionIDs[id] = true
	}
	memberRows.Close()

	inviteRows, _ := app.db.Query(context.Background(), "SELECT invitee_id FROM board_invitations WHERE board_id = $1 AND status = 'pending'", boardID)
	for inviteRows.Next() {
		var id string
		inviteRows.Scan(&id)
		exclusionIDs[id] = true
	}
	inviteRows.Close()

	invitableUsers := make([]User, 0)
	for _, user := range allUsers {
		if !exclusionIDs[user.ID] {
			invitableUsers = append(invitableUsers, user)
		}
	}

	return c.JSON(invitableUsers)
}

// convidar user
func (app *App) inviteUserToBoard(c *fiber.Ctx) error {
	boardID, _ := strconv.Atoi(c.Params("id"))
	inviterID := c.Locals("userID").(string)
	var payload struct {
		InviteeID string `json:"invitee_id"`
	}
	if err := c.BodyParser(&payload); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Payload inválido"})
	}

	var existingCount int
	checkQuery := `
		SELECT COUNT(*) FROM (
			SELECT owner_id as user_id FROM boards WHERE id = $1
			UNION
			SELECT user_id FROM board_memberships WHERE board_id = $1
			UNION
			SELECT invitee_id FROM board_invitations WHERE board_id = $1 AND status = 'pending'
		) as excluded_users WHERE user_id = $2
	`
	err := app.db.QueryRow(context.Background(), checkQuery, boardID, payload.InviteeID).Scan(&existingCount)
	if err != nil {
		log.Printf("Erro ao verificar usuário existente para convite: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao verificar permissões de convite"})
	}

	if existingCount > 0 {
		return c.Status(200).JSON(fiber.Map{"status": "invited_or_exists", "message": "Este usuário já é membro ou tem um convite pendente."})
	}

	tx, err := app.db.Begin(context.Background())
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao iniciar transação"})
	}
	defer tx.Rollback(context.Background())

	var invID int
	invQuery := `INSERT INTO board_invitations (board_id, inviter_id, invitee_id, status) VALUES ($1, $2, $3, 'pending') RETURNING id`
	err = tx.QueryRow(context.Background(), invQuery, boardID, inviterID, payload.InviteeID).Scan(&invID)
	if err != nil {
		log.Printf("Erro ao inserir convite na DB: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao criar o novo convite"})
	}

	var boardTitle string
	tx.QueryRow(context.Background(), "SELECT title FROM boards WHERE id = $1", boardID).Scan(&boardTitle)

	inviterName := app.getDisplayName(context.Background(), tx, inviterID)

	notification := Notification{
		UserID:         payload.InviteeID,
		Type:           "board_invitation",
		Message:        fmt.Sprintf("%s convidou você para o quadro '%s'", inviterName, boardTitle),
		RelatedBoardID: &boardID,
		InvitationID:   &invID,
	}
	if err := app.createNotification(tx, notification); err != nil {
		log.Printf("Erro ao criar notificação na DB: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao criar notificação"})
	}

	if err := tx.Commit(context.Background()); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao confirmar convite"})
	}

	return c.Status(201).JSON(fiber.Map{"status": "invited"})
}

// responder convite
func (app *App) respondToInvitation(c *fiber.Ctx) error {
	invitationID, _ := strconv.Atoi(c.Params("id"))
	notificationID_str := c.Query("notification_id")
	notificationID, _ := strconv.Atoi(notificationID_str)
	userID := c.Locals("userID").(string)

	var payload struct {
		Accept bool `json:"accept"`
	}
	if err := c.BodyParser(&payload); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Payload inválido"})
	}

	tx, err := app.db.Begin(context.Background())
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao iniciar transação"})
	}
	defer tx.Rollback(context.Background())

	var boardID int
	var ownerID string
	var boardTitle string
	status := "rejected"
	if payload.Accept {
		status = "accepted"
	}

	err = tx.QueryRow(context.Background(), "UPDATE board_invitations SET status = $1, updated_at = now() WHERE id = $2 AND invitee_id = $3 RETURNING board_id", status, invitationID, userID).Scan(&boardID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(404).JSON(fiber.Map{"error": "Convite não encontrado ou não pertence a você"})
		}
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao atualizar convite"})
	}

	if payload.Accept {
		_, err = tx.Exec(context.Background(), "INSERT INTO board_memberships (board_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", boardID, userID)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Erro ao adicionar membro ao quadro"})
		}

		inviteeName := app.getDisplayName(context.Background(), tx, userID)

		tx.QueryRow(context.Background(), "SELECT owner_id, title FROM boards WHERE id = $1", boardID).Scan(&ownerID, &boardTitle)

		if ownerID != "" && inviteeName != "" {
			ownerNotification := Notification{
				UserID:         ownerID,
				Type:           "invitation_accepted",
				Message:        fmt.Sprintf("%s aceitou seu convite para o quadro '%s'.", inviteeName, boardTitle),
				RelatedBoardID: &boardID,
			}
			app.createNotification(tx, ownerNotification)
		}
	}

	if notificationID > 0 {
		_, err = tx.Exec(context.Background(), "UPDATE notifications SET is_read = true WHERE id = $1 AND user_id = $2", notificationID, userID)
		if err != nil {
			log.Printf("Aviso: Falha ao marcar notificação %d como lida para o usuário %s: %v", notificationID, userID, err)
		}
	}

	if err := tx.Commit(context.Background()); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao confirmar resposta"})
	}
	return c.Status(200).JSON(fiber.Map{"status": "responded"})
}

// pegar membros board
func (app *App) getBoardMembers(c *fiber.Ctx) error {
	boardID, _ := strconv.Atoi(c.Params("id"))
	userID := c.Locals("userID").(string)
	hasPermission, err := app.checkBoardPermission(userID, boardID)
	if err != nil || !hasPermission {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Acesso negado."})
	}
	query := `(SELECT u.id, u.email, COALESCE(u.raw_user_meta_data->>'username', u.email) as username, COALESCE(u.raw_user_meta_data->>'avatar_url', '') as avatar, true as is_owner FROM auth.users u JOIN boards b ON u.id = b.owner_id WHERE b.id = $1) UNION (SELECT u.id, u.email, COALESCE(u.raw_user_meta_data->>'username', u.email) as username, COALESCE(u.raw_user_meta_data->>'avatar_url', '') as avatar, false as is_owner FROM auth.users u JOIN board_memberships bm ON u.id = bm.user_id WHERE bm.board_id = $1 AND u.id NOT IN (SELECT owner_id FROM boards WHERE id = $1)) ORDER BY is_owner DESC, username;`
	rows, err := app.db.Query(context.Background(), query, boardID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao buscar membros"})
	}
	defer rows.Close()
	type Member struct {
		User
		IsOwner bool `json:"is_owner"`
	}
	members := make([]Member, 0)
	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.ID, &member.Email, &member.Username, &member.Avatar, &member.IsOwner); err == nil {
			members = append(members, member)
		}
	}
	return c.JSON(members)
}

// pegar notificacoes
func (app *App) getNotifications(c *fiber.Ctx) error {
	userID := c.Locals("userID").(string)
	query := `
		SELECT 
			n.id, n.user_id, n.type, n.message, n.is_read, n.related_board_id, n.related_card_id, 
			n.invitation_id, n.created_at
		FROM notifications n
		WHERE n.user_id = $1
		ORDER BY n.is_read ASC, n.created_at DESC
	`
	rows, err := app.db.Query(context.Background(), query, userID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao buscar notificações"})
	}
	defer rows.Close()
	notifications := make([]Notification, 0)
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.UserID, &n.Type, &n.Message, &n.IsRead, &n.RelatedBoardID, &n.RelatedCardID, &n.InvitationID, &n.CreatedAt); err == nil {
			notifications = append(notifications, n)
		}
	}
	return c.JSON(notifications)
}

// func notificacao lida
func (app *App) markNotificationRead(c *fiber.Ctx) error {
	notificationID, _ := strconv.Atoi(c.Params("id"))
	userID := c.Locals("userID").(string)
	_, err := app.db.Exec(context.Background(), "UPDATE notifications SET is_read = true WHERE id = $1 AND user_id = $2", notificationID, userID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Erro ao marcar notificação como lida"})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (app *App) markAllNotificationsRead(c *fiber.Ctx) error {
	userID, ok := c.Locals("userID").(string)
	if !ok || userID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "ID do usuário não pôde ser verificado"})
	}

	query := `UPDATE notifications SET is_read = TRUE WHERE user_id = $1 AND is_read = FALSE`

	cmdTag, err := app.db.Exec(context.Background(), query, userID)
	if err != nil {
		log.Printf("❌ Erro ao marcar todas as notificações como lidas para o usuário %s: %v", userID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Erro interno ao atualizar as notificações"})
	}

	log.Printf("", cmdTag.RowsAffected(), userID)

	return c.SendStatus(fiber.StatusNoContent)
}

// remover membro board
func (app *App) removeBoardMember(c *fiber.Ctx) error {
	boardID, err := strconv.Atoi(c.Params("boardId"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "ID do quadro inválido"})
	}
	memberIdToRemove := c.Params("memberId")
	currentUserID := c.Locals("userID").(string)

	var ownerID string
	err = app.db.QueryRow(context.Background(), "SELECT owner_id FROM boards WHERE id = $1", boardID).Scan(&ownerID)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Quadro não encontrado"})
	}

	if ownerID != currentUserID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "Apenas o dono do quadro pode remover membros."})
	}

	if ownerID == memberIdToRemove {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "O dono do quadro não pode ser removido."})
	}

	_, err = app.db.Exec(context.Background(), "DELETE FROM board_memberships WHERE board_id = $1 AND user_id = $2", boardID, memberIdToRemove)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Falha ao remover o membro do banco de dados."})
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// MAIN
func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Arquivo .env não encontrado, usando variáveis de ambiente do sistema.")
	}

	app := &App{clients: make(map[int]map[*websocket.Conn]bool)}
	app.colLocks.locks = make(map[int]*sync.Mutex)

	if err := app.connectDB(); err != nil {
		log.Fatalf("Falha ao conectar ao banco de dados: %v", err)
	}
	defer app.db.Close()

	engine := html.NewFileSystem(http.FS(templates), ".html")
	fiberApp := fiber.New(fiber.Config{Views: engine})
	fiberApp.Use(logger.New(), recover.New())
	fiberApp.Use(cors.New(cors.Config{
		AllowOrigins:     "https://nm-kanban-api.onrender.com, http://localhost:8080, http://127.0.0.1:8080",
		AllowCredentials: true,
		AllowMethods:     "GET,POST,PUT,DELETE,OPTIONS",
		AllowHeaders:     "Origin,Content-Type,Accept,Authorization",
	}))
	app.setupRoutes(fiberApp)
	fiberApp.Use("/static", filesystem.New(filesystem.Config{
		Root:       http.FS(static),
		PathPrefix: "frontend/static",
	}))
	fiberApp.Get("/ws/board/:id", websocket.New(app.handleWebSocket))
	fiberApp.Get("/*", func(c *fiber.Ctx) error {
		return c.Render("frontend/templates/index", fiber.Map{"Title": "NM Kanban"})
	})
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := fmt.Sprintf("0.0.0.0:%s", port)
	log.Printf("Servidor iniciando na porta %s", port)
	if err := fiberApp.Listen(addr); err != nil {
		log.Fatalf("Erro ao iniciar o servidor: %v", err)
	}
}
