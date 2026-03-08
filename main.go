package main

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// DBインスタンスをグローバル変数として定義
// グローバル変数定数
var db *gorm.DB

// HTTPクライアントを再利用（タイムアウト設定付き）
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

// 定数定義
const defaultGeminiModel = "gemini-2.5-flash"
const defaultPort = "8080"

type Folder struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"column:name" json:"name"`
	ParentID  *uint     `gorm:"column:parent_id" json:"parent_id"`
	SortOrder int       `gorm:"column:sort_order;default:0" json:"sort_order"`
	Children  []*Folder `gorm:"-" json:"children,omitempty"`
}

type Department struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"column:name" json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type User struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Name         string    `gorm:"column:name" json:"name"`
	Email        string    `gorm:"column:email;uniqueIndex" json:"email"`
	Password     string    `gorm:"column:password" json:"-"` // レスポンスには含めない
	Role         string    `gorm:"column:role" json:"role"`  // 'sales' | 'legal' | 'legal_manager'
	DepartmentID *uint     `gorm:"column:department_id" json:"department_id"`
	IsActive     bool      `gorm:"column:is_active;default:true" json:"is_active"`
	CreatedAt    time.Time `json:"created_at"`
}

type ContractFile struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	ContractID uint      `gorm:"column:contract_id" json:"contract_id"`
	Version    int       `gorm:"column:version" json:"version"`
	FileType   string    `gorm:"column:file_type" json:"file_type"` // 'pdf' | 'word'
	Path       string    `gorm:"column:path" json:"path"`
	Filename   string    `gorm:"column:filename" json:"filename"`
	UploadedBy *uint     `gorm:"column:uploaded_by" json:"uploaded_by"`
	CreatedAt  time.Time `json:"created_at"`
}

type ReviewHistory struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	ContractID uint      `gorm:"column:contract_id" json:"contract_id"`
	UserID     uint      `gorm:"column:user_id" json:"user_id"`
	Action     string    `gorm:"column:action" json:"action"`
	Comment    string    `gorm:"column:comment" json:"comment"`
	CreatedAt  time.Time `json:"created_at"`
}

type Contract struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	Title         string     `gorm:"column:title" json:"title" form:"title"`
	ClientName    string     `gorm:"column:client_name" json:"client_name" form:"client_name"`
	Amount        int        `gorm:"column:amount" json:"amount" form:"amount"`
	ContractDate  string     `gorm:"column:contract_date" json:"contract_date" form:"contract_date"` // 契約締結日
	StartDate     string     `gorm:"column:start_date" json:"start_date" form:"start_date"`         // 契約開始日
	EndDate       string     `gorm:"column:end_date" json:"end_date" form:"end_date"`               // 契約終了日
	AutoRenewal   bool       `gorm:"column:auto_renewal" json:"auto_renewal" form:"auto_renewal"`   // 自動更新有無
	PDFPath       string     `gorm:"column:pdf_path" json:"pdf_path"`
	PDFFilename   string     `gorm:"column:pdf_filename" json:"pdf_filename"`
	FolderID      *uint      `gorm:"column:folder_id" json:"folder_id" form:"folder_id"`
	Summary       string     `gorm:"column:summary" json:"summary" form:"summary"`
	Status        string     `gorm:"column:status;default:draft" json:"status"`
	RequesterID   *uint      `gorm:"column:requester_id" json:"requester_id"`
	AssigneeID    *uint      `gorm:"column:assignee_id" json:"assignee_id"`
	DepartmentID  *uint      `gorm:"column:department_id" json:"department_id"`
	RequestedAt   *time.Time `gorm:"column:requested_at" json:"requested_at"`
	DueDate       *time.Time `gorm:"column:due_date" json:"due_date"`
	ReviewVersion int        `gorm:"column:review_version;default:0" json:"review_version"`
}

func main() {
	// .envファイルを読み込む（存在する場合）
	loadEnv()
	initDB()
	ensureUploadsDir()

	r := setupRouter()

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	log.Printf("Server is running on port %s", port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// loadEnv は標準ライブラリのみで.envファイルを読み込みます
func loadEnv() {
	file, err := os.Open(".env")
	if err != nil {
		return // .envがない場合は無視
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			if os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
	}
}

func initDB() {
	var err error
	// パフォーマンス最適化: WALモード有効化、タイムアウト設定
	dsn := "test.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous=NORMAL"
	db, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{
		PrepareStmt:            true, // ステートメントキャッシュを有効化
		SkipDefaultTransaction: true, // 単一操作のトランザクションをスキップして高速化
	})
	if err != nil {
		log.Fatalf("データベースに接続できませんでした: %v", err)
	}

	// コネクションプールの設定
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(time.Hour)

	db.AutoMigrate(&Contract{}, &Folder{}, &Department{}, &User{}, &ContractFile{}, &ReviewHistory{})
	migrateExistingPDFs()
	seedDefaultFolders()
	seedAdminUser()
}

func migrateExistingPDFs() {
	// 移行済みかどうかを contract_files にレコードが1件以上あるかで判定
	var count int64
	db.Model(&ContractFile{}).Count(&count)
	if count > 0 {
		return
	}

	// pdf_path が設定されている契約を取得
	var contracts []Contract
	db.Where("pdf_path != ''").Find(&contracts)
	if len(contracts) == 0 {
		return
	}

	for _, c := range contracts {
		cf := ContractFile{
			ContractID: c.ID,
			Version:    1,
			FileType:   "pdf",
			Path:       c.PDFPath,
			Filename:   c.PDFFilename,
		}
		if err := db.Create(&cf).Error; err != nil {
			log.Printf("既存PDFの移行に失敗しました (contract_id=%d): %v", c.ID, err)
		}
	}
	log.Printf("既存PDFを contract_files テーブルに移行しました（%d件）", len(contracts))
}

func ensureUploadsDir() {
	// アップロード保存用のディレクトリを作成
	os.MkdirAll("uploads", 0755)
}

// --- JWT認証 ---

// jwtClaims はJWTのペイロード構造体
type jwtClaims struct {
	UserID       uint   `json:"user_id"`
	Email        string `json:"email"`
	Role         string `json:"role"`
	DepartmentID *uint  `json:"department_id"`
	Exp          int64  `json:"exp"`
}

// base64urlEncode はRFC 4648 §5のbase64url（パディングなし）エンコード
func base64urlEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// generateToken はHS256署名のJWTを生成する
func generateToken(user User) (string, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return "", fmt.Errorf("JWT_SECRET environment variable is not set")
	}

	// Header
	headerJSON, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	header := base64urlEncode(headerJSON)

	// Payload
	claims := jwtClaims{
		UserID:       user.ID,
		Email:        user.Email,
		Role:         user.Role,
		DepartmentID: user.DepartmentID,
		Exp:          time.Now().Add(24 * time.Hour).Unix(),
	}
	payloadJSON, _ := json.Marshal(claims)
	payload := base64urlEncode(payloadJSON)

	// Signature
	signingInput := header + "." + payload
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	sig := base64urlEncode(mac.Sum(nil))

	return signingInput + "." + sig, nil
}

// verifyToken はJWTを検証してclaimsを返す
func verifyToken(tokenStr string) (*jwtClaims, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return nil, fmt.Errorf("JWT_SECRET environment variable is not set")
	}

	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token format")
	}

	// 署名を検証
	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	expectedSig := base64urlEncode(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
		return nil, fmt.Errorf("invalid token signature")
	}

	// ペイロードをデコード
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid token payload")
	}

	var claims jwtClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("failed to parse token payload")
	}

	// 有効期限チェック
	if time.Now().Unix() > claims.Exp {
		return nil, fmt.Errorf("token expired")
	}

	return &claims, nil
}

// AuthMiddleware はBearerトークンを検証してContextにユーザー情報をセットする
func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "認証トークンがありません"})
			return
		}

		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		claims, err := verifyToken(tokenStr)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "無効なトークンです: " + err.Error()})
			return
		}

		c.Set("user_id", claims.UserID)
		c.Set("role", claims.Role)
		c.Set("department_id", claims.DepartmentID)
		c.Next()
	}
}

// RequireRole は指定ロールを持つユーザーのみ通過を許可する（AuthMiddlewareの後に使う）
func RequireRole(roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		role, _ := c.Get("role")
		roleStr, _ := role.(string)
		for _, r := range roles {
			if r == roleStr {
				c.Next()
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "この操作を行う権限がありません"})
	}
}

// --- 認証ハンドラー ---

func loginHandler(c *gin.Context) {
	var input struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var user User
	if err := db.Where("email = ? AND is_active = ?", input.Email, true).First(&user).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "メールアドレスまたはパスワードが正しくありません"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(input.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "メールアドレスまたはパスワードが正しくありません"})
		return
	}

	token, err := generateToken(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "トークンの生成に失敗しました"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token": token,
		"user": gin.H{
			"id":            user.ID,
			"name":          user.Name,
			"email":         user.Email,
			"role":          user.Role,
			"department_id": user.DepartmentID,
		},
	})
}

func meHandler(c *gin.Context) {
	userID, _ := c.Get("user_id")
	var user User
	if err := db.First(&user, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ユーザーが見つかりません"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":            user.ID,
		"name":          user.Name,
		"email":         user.Email,
		"role":          user.Role,
		"department_id": user.DepartmentID,
	})
}

func seedDefaultFolders() {
	var count int64
	db.Model(&Folder{}).Count(&count)
	if count == 0 {
		// 究極に使いやすい構成として、3つの主要カテゴリを作成
		// これにより「取引先別」「契約種別」「年度別」の3軸で管理が可能になります
		defaultFolders := []string{"取引先別", "契約種別", "年度別"}
		for i, name := range defaultFolders {
			db.Create(&Folder{Name: name, SortOrder: i + 1})
		}
		log.Println("初期フォルダ構成（取引先別、契約種別、年度別）を作成しました")
	}
}

func seedAdminUser() {
	var count int64
	db.Model(&User{}).Count(&count)
	if count > 0 {
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte("admin1234"), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("管理者ユーザーのパスワードハッシュ化に失敗しました: %v", err)
		return
	}
	admin := User{
		Name:     "システム管理者",
		Email:    "admin@example.com",
		Password: string(hashed),
		Role:     "legal_manager",
		IsActive: true,
	}
	if err := db.Create(&admin).Error; err != nil {
		log.Printf("管理者ユーザーの作成に失敗しました: %v", err)
		return
	}
	log.Println("初期管理者ユーザー (admin@example.com) を作成しました")
}

// --- 部署ハンドラー ---

func getDepartmentsHandler(c *gin.Context) {
	var departments []Department
	if err := db.Find(&departments).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "部署の取得に失敗しました"})
		return
	}
	c.JSON(http.StatusOK, departments)
}

func createDepartmentHandler(c *gin.Context) {
	var dept Department
	if err := c.ShouldBindJSON(&dept); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := db.Create(&dept).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "部署の作成に失敗しました"})
		return
	}
	c.JSON(http.StatusCreated, dept)
}

func updateDepartmentHandler(c *gin.Context) {
	id := c.Param("id")
	var dept Department
	if err := db.First(&dept, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "部署が見つかりません"})
		return
	}
	var input map[string]interface{}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := db.Model(&dept).Updates(input).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "部署の更新に失敗しました"})
		return
	}
	db.First(&dept, id)
	c.JSON(http.StatusOK, dept)
}

// --- ユーザーハンドラー ---

func getUsersHandler(c *gin.Context) {
	var users []User
	if err := db.Find(&users).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ユーザーの取得に失敗しました"})
		return
	}
	c.JSON(http.StatusOK, users)
}

func createUserHandler(c *gin.Context) {
	var input struct {
		Name         string `json:"name" binding:"required"`
		Email        string `json:"email" binding:"required"`
		Password     string `json:"password" binding:"required"`
		Role         string `json:"role" binding:"required"`
		DepartmentID *uint  `json:"department_id"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "パスワードのハッシュ化に失敗しました"})
		return
	}

	user := User{
		Name:         input.Name,
		Email:        input.Email,
		Password:     string(hashed),
		Role:         input.Role,
		DepartmentID: input.DepartmentID,
		IsActive:     true,
	}
	if err := db.Create(&user).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ユーザーの作成に失敗しました"})
		return
	}
	c.JSON(http.StatusCreated, user)
}

func updateUserHandler(c *gin.Context) {
	id := c.Param("id")
	var target User
	if err := db.First(&target, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ユーザーが見つかりません"})
		return
	}

	callerRole, _ := c.Get("role")
	callerRoleStr, _ := callerRole.(string)
	callerID, _ := c.Get("user_id")
	callerIDUint, _ := callerID.(uint)

	var input map[string]interface{}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// legal_manager 以外が自分以外を更新しようとした場合は禁止
	if callerRoleStr != "legal_manager" && callerIDUint != target.ID {
		c.JSON(http.StatusForbidden, gin.H{"error": "他のユーザーを更新する権限がありません"})
		return
	}

	// legal_manager 以外は name と password 以外の変更を禁止
	if callerRoleStr != "legal_manager" {
		for key := range input {
			if key != "name" && key != "password" {
				delete(input, key)
			}
		}
	}

	// password が含まれる場合は bcrypt ハッシュ化
	if pw, ok := input["password"].(string); ok && pw != "" {
		hashed, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "パスワードのハッシュ化に失敗しました"})
			return
		}
		input["password"] = string(hashed)
	} else {
		delete(input, "password")
	}

	if err := db.Model(&target).Updates(input).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ユーザーの更新に失敗しました"})
		return
	}
	db.First(&target, id)
	c.JSON(http.StatusOK, target)
}

func setupRouter() *gin.Engine {
	r := gin.Default()

	// 静的ファイル
	r.StaticFile("/", "./index.html")
	r.StaticFile("/index.html", "./index.html")
	r.StaticFile("/contract_detail.html", "./contract_detail.html")
	r.Static("/uploads", "./uploads")

	// 認証不要エンドポイント
	r.GET("/hello", helloHandler)
	r.POST("/auth/login", loginHandler)

	// 認証必須エンドポイント
	auth := r.Group("/", AuthMiddleware())
	{
		auth.POST("/auth/me", meHandler)

		// 契約関連
		auth.GET("/contracts", getContractsHandler)
		auth.GET("/contracts/:id", getContractDetailHandler)
		auth.POST("/contracts", createContractHandler)
		auth.DELETE("/contracts/:id", deleteContractHandler)
		auth.PUT("/contracts/:id", updateContractHandler)
		auth.POST("/contracts/:id/analyze", reanalyzeContractHandler)

		// AI解析
		auth.POST("/api/analyze", analyzeContractHandler)

		// フォルダ関連
		auth.GET("/folders", getFoldersHandler)
		auth.POST("/folders", createFolderHandler)
		auth.PUT("/folders/:id", updateFolderHandler)
		auth.DELETE("/folders/:id", deleteFolderHandler)

		// 部署関連（GET は全ロール可、POST/PUT は legal_manager のみ）
		auth.GET("/departments", getDepartmentsHandler)
		auth.POST("/departments", RequireRole("legal_manager"), createDepartmentHandler)
		auth.PUT("/departments/:id", RequireRole("legal_manager"), updateDepartmentHandler)

		// ユーザー関連（GET/POST は legal_manager のみ、PUT は権限チェックをハンドラー内で行う）
		auth.GET("/users", RequireRole("legal_manager"), getUsersHandler)
		auth.POST("/users", RequireRole("legal_manager"), createUserHandler)
		auth.PUT("/users/:id", updateUserHandler)
	}

	return r
}

// --- ハンドラー関数 ---

func helloHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"message": "Hello, Go World!"})
}

func getContractsHandler(c *gin.Context) {
	var contracts []Contract
	query := db.Model(&Contract{})

	// sales ロールは自分の部署の契約のみ参照可
	if role, _ := c.Get("role"); role == "sales" {
		if deptID, exists := c.Get("department_id"); exists && deptID != nil {
			query = query.Where("department_id = ?", deptID)
		} else {
			// 部署未設定の sales は何も返さない
			query = query.Where("1 = 0")
		}
	}

	// フィルタリング
	if title := c.Query("title"); title != "" {
		query = query.Where("title LIKE ?", "%"+title+"%")
	}
	if clientName := c.Query("client_name"); clientName != "" {
		query = query.Where("client_name LIKE ?", "%"+clientName+"%")
	}
	if folderID := c.Query("folder_id"); folderID != "" {
		if folderID == "0" {
			query = query.Where("folder_id IS NULL")
		} else {
			query = query.Where("folder_id = ?", folderID)
		}
	}

	query.Find(&contracts)
	c.JSON(http.StatusOK, contracts)
}

func getContractDetailHandler(c *gin.Context) {
	id := c.Param("id")
	var contract Contract
	if err := db.First(&contract, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "契約が見つかりません"})
		return
	}
	c.JSON(http.StatusOK, contract)
}

func createContractHandler(c *gin.Context) {
	var newContract Contract
	// バインディングを使用してフォームデータを構造体にマッピング
	if err := c.ShouldBind(&newContract); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// folder_idが0の場合はNULL（ルートフォルダ）として扱う
	if newContract.FolderID != nil && *newContract.FolderID == 0 {
		newContract.FolderID = nil
	}

	// Context から requester_id / department_id を自動セット
	if uid, exists := c.Get("user_id"); exists {
		if v, ok := uid.(uint); ok {
			newContract.RequesterID = &v
		}
	}
	if deptID, exists := c.Get("department_id"); exists && deptID != nil {
		if v, ok := deptID.(uint); ok {
			newContract.DepartmentID = &v
		}
	}
	// status は常に draft で登録
	newContract.Status = "draft"

	var pdfPath, pdfFilename string

	file, err := c.FormFile("pdf")
	if err == nil {
		filename := fmt.Sprintf("%d-%s", time.Now().Unix(), file.Filename)
		savePath := filepath.Join("uploads", filename)

		if err := c.SaveUploadedFile(file, savePath); err == nil {
			pdfPath = "/uploads/" + filename
			pdfFilename = file.Filename
		} else {
			log.Printf("Failed to save file: %v", err)
		}
	}

	newContract.PDFPath = pdfPath
	newContract.PDFFilename = pdfFilename

	db.Create(&newContract)
	c.JSON(http.StatusCreated, newContract)
}

func deleteContractHandler(c *gin.Context) {
	id := c.Param("id")

	role, _ := c.Get("role")
	roleStr, _ := role.(string)

	// legal は削除不可
	if roleStr == "legal" {
		c.JSON(http.StatusForbidden, gin.H{"error": "削除権限がありません"})
		return
	}

	var contract Contract
	if err := db.First(&contract, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "契約が見つかりません"})
		return
	}

	// sales は自分が依頼した draft 契約のみ削除可
	if roleStr == "sales" {
		callerID, _ := c.Get("user_id")
		callerIDUint, _ := callerID.(uint)
		if contract.RequesterID == nil || *contract.RequesterID != callerIDUint {
			c.JSON(http.StatusForbidden, gin.H{"error": "自分が依頼した契約のみ削除できます"})
			return
		}
		if contract.Status != "draft" {
			c.JSON(http.StatusForbidden, gin.H{"error": "draft 状態の契約のみ削除できます"})
			return
		}
	}

	if err := db.Delete(&contract).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "削除に失敗しました"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "契約ID " + id + " を削除しました"})
}

func updateContractHandler(c *gin.Context) {
	id := c.Param("id")
	var contract Contract

	if err := db.First(&contract, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "指定された契約が見つかりません"})
		return
	}

	var input map[string]interface{}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// status の直接変更は全ロール禁止
	delete(input, "status")

	role, _ := c.Get("role")
	roleStr, _ := role.(string)

	switch roleStr {
	case "sales":
		// draft / rejected のみ編集可
		if contract.Status != "draft" && contract.Status != "rejected" {
			c.JSON(http.StatusForbidden, gin.H{"error": "draft または rejected の契約のみ編集できます"})
			return
		}
		allowed := map[string]bool{
			"title": true, "client_name": true, "amount": true,
			"contract_date": true, "start_date": true, "end_date": true,
			"auto_renewal": true, "summary": true, "folder_id": true, "due_date": true,
		}
		for key := range input {
			if !allowed[key] {
				delete(input, key)
			}
		}
	case "legal", "legal_manager":
		allowed := map[string]bool{
			"title": true, "client_name": true, "amount": true,
			"contract_date": true, "start_date": true, "end_date": true,
			"auto_renewal": true, "summary": true, "folder_id": true, "assignee_id": true,
		}
		for key := range input {
			if !allowed[key] {
				delete(input, key)
			}
		}
	}

	// folder_idが0の場合はNULL（ルートフォルダ）として扱う
	if v, ok := input["folder_id"]; ok {
		if f, ok := v.(float64); ok && f == 0 {
			input["folder_id"] = nil
		}
	}

	if err := db.Model(&contract).Updates(input).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新に失敗しました"})
		return
	}

	// 更新後のデータを再取得
	db.First(&contract, id)
	c.JSON(http.StatusOK, contract)
}

// --- AI解析ハンドラー ---

func extractContractInfo(fileBytes []byte) (map[string]interface{}, error) {
	// 環境変数からAPIキーを取得
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY environment variable is not set")
	}

	// Gemini APIへのリクエスト構築
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", defaultGeminiModel)

	prompt := `
あなたは契約書解析のアシスタントです。添付されたPDFファイルを解析し、以下の情報をJSON形式で抽出してください。
Markdownのコードブロックは含めず、生のJSONのみを返してください。

抽出項目:
- title (string): 契約書のタイトル
- client_name (string): 取引先・相手方の名称
- amount (int): 契約金額（数値のみ、見つからない場合は0）
- contract_date (string): 契約締結日 (YYYY-MM-DD形式)
- start_date (string): 契約開始日 (YYYY-MM-DD形式)
- end_date (string): 契約終了日 (YYYY-MM-DD形式)
- auto_renewal (boolean): 自動更新の有無 (true/false)
- summary (string): 契約書の要約（主要な条項や権利義務関係を含む要約）
`

	requestBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]interface{}{
					{"text": prompt},
					{
						"inline_data": map[string]string{
							"mime_type": "application/pdf",
							"data":      base64.StdEncoding.EncodeToString(fileBytes),
						},
					},
				},
			},
		},
	}

	jsonData, _ := json.Marshal(requestBody)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Failed to call AI API: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 429 {
			return nil, fmt.Errorf("AI API Rate Limit Exceeded (429). Details: %s", string(body))
		}
		return nil, fmt.Errorf("AI API Error (Status: %d): %s", resp.StatusCode, string(body))
	}

	// レスポンスの解析
	var geminiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	// 読み込んだbodyをデコードに使用
	if err := json.Unmarshal(body, &geminiResp); err != nil {
		return nil, fmt.Errorf("Failed to parse AI response: %v", err)
	}

	if len(geminiResp.Candidates) > 0 && len(geminiResp.Candidates[0].Content.Parts) > 0 {
		text := geminiResp.Candidates[0].Content.Parts[0].Text
		// Markdownのコードブロックを除去
		text = strings.TrimSpace(text)
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)

		// JSONとしてそのままクライアントに返す（クライアント側でキーが一致していれば自動入力される）
		var result map[string]interface{}
		if err := json.Unmarshal([]byte(text), &result); err == nil {
			return result, nil
		} else {
			return nil, fmt.Errorf("JSON Parse Error: %v\nRaw Text: %s", err, text)
		}
	}

	return nil, fmt.Errorf("Could not extract data")
}

func analyzeContractHandler(c *gin.Context) {
	file, err := c.FormFile("pdf")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "PDF file is required"})
		return
	}

	f, err := file.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open file"})
		return
	}
	defer f.Close()

	fileBytes, err := io.ReadAll(f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read file"})
		return
	}

	result, err := extractContractInfo(fileBytes)
	if err != nil {
		fmt.Println(err)
		log.Printf("Analysis error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

func reanalyzeContractHandler(c *gin.Context) {
	id := c.Param("id")
	var contract Contract
	if err := db.First(&contract, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "契約が見つかりません"})
		return
	}

	if contract.PDFPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "PDFファイルが登録されていません"})
		return
	}

	// PDFPathは "/uploads/..." なので先頭の "/" を削除して相対パスにする
	filePath := strings.TrimPrefix(contract.PDFPath, "/")
	fileBytes, err := os.ReadFile(filePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ファイルの読み込みに失敗しました: " + err.Error()})
		return
	}

	result, err := extractContractInfo(fileBytes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// --- フォルダ用ハンドラー関数 ---

func getFoldersHandler(c *gin.Context) {
	var allFolders []*Folder
	if err := db.Order("sort_order asc").Find(&allFolders).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "フォルダの取得に失敗しました"})
		return
	}

	// メモリ割り当ての最適化
	folderMap := make(map[uint]*Folder, len(allFolders))
	for _, f := range allFolders {
		folderMap[f.ID] = f
	}

	var rootFolders []*Folder
	for _, f := range allFolders {
		if f.ParentID == nil {
			rootFolders = append(rootFolders, f)
		} else {
			if parent, ok := folderMap[*f.ParentID]; ok {
				parent.Children = append(parent.Children, f)
			}
		}
	}
	c.JSON(http.StatusOK, rootFolders)
}

func createFolderHandler(c *gin.Context) {
	var folder Folder
	if err := c.ShouldBindJSON(&folder); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// parent_idが0の場合はルートフォルダとみなす
	if folder.ParentID != nil && *folder.ParentID == 0 {
		folder.ParentID = nil
	}

	// 階層制限のチェック（管理しやすくするため3階層までに制限）
	// 1階層目（ルート）と2階層目の配下には作成可能 -> 最大で第3階層まで
	if folder.ParentID != nil {
		var parent Folder
		if err := db.First(&parent, *folder.ParentID).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "親フォルダが見つかりません"})
			return
		}

		// 親の深さを確認
		depth := 0
		current := parent
		for current.ParentID != nil {
			depth++
			var grandParent Folder
			if err := db.First(&grandParent, *current.ParentID).Error; err != nil {
				break
			}
			current = grandParent
		}

		// 親がすでに深さ1（第2階層）の場合、その下（第3階層）まではOKだが、それ以上はNGとする
		if depth >= 2 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "フォルダは3階層まで（ルート > 第2階層 > 第3階層）に制限されています"})
			return
		}
	}

	db.Create(&folder)
	c.JSON(http.StatusCreated, folder)
}

func updateFolderHandler(c *gin.Context) {
	id := c.Param("id")
	var folder Folder
	if err := db.First(&folder, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "フォルダが見つかりません"})
		return
	}
	var input map[string]interface{}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// parent_idが0の場合はルートフォルダとみなす
	if v, ok := input["parent_id"]; ok {
		if f, ok := v.(float64); ok && f == 0 {
			input["parent_id"] = nil
		}
	}

	db.Model(&folder).Updates(input)
	db.First(&folder, id) // 更新後のデータを再取得
	c.JSON(http.StatusOK, folder)
}

func deleteFolderHandler(c *gin.Context) {
	id := c.Param("id")

	// 子フォルダや契約書が存在する場合は削除を禁止（誤操作防止）
	var childCount int64
	db.Model(&Folder{}).Where("parent_id = ?", id).Count(&childCount)
	if childCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "子フォルダが存在するため削除できません"})
		return
	}

	var contractCount int64
	db.Model(&Contract{}).Where("folder_id = ?", id).Count(&contractCount)
	if contractCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "フォルダ内に契約書が存在するため削除できません"})
		return
	}

	if err := db.Delete(&Folder{}, id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "削除に失敗しました"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "フォルダID " + id + " を削除しました"})
}