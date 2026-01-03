package main

import (
	"bufio"
	"bytes"
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
	ID       uint   `gorm:"primaryKey" json:"id"`
	Name     string `gorm:"column:name" json:"name"`
	ParentID *uint  `gorm:"column:parent_id" json:"parent_id"`
	SortOrder int    `gorm:"column:sort_order;default:0" json:"sort_order"`
	Children []*Folder `gorm:"-" json:"children,omitempty"`
}

type Contract struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	Title        string `gorm:"column:title" json:"title" form:"title"`
	ClientName   string `gorm:"column:client_name" json:"client_name" form:"client_name"`
	Amount       int    `gorm:"column:amount" json:"amount" form:"amount"`
	ContractDate string `gorm:"column:contract_date" json:"contract_date" form:"contract_date"` // 契約締結日
	StartDate    string `gorm:"column:start_date" json:"start_date" form:"start_date"`       // 契約開始日
	EndDate      string `gorm:"column:end_date" json:"end_date" form:"end_date"`           // 契約終了日
	AutoRenewal  bool   `gorm:"column:auto_renewal" json:"auto_renewal" form:"auto_renewal"`   // 自動更新有無
	PDFPath      string `gorm:"column:pdf_path" json:"pdf_path"`
	PDFFilename  string `gorm:"column:pdf_filename" json:"pdf_filename"`
	FolderID     *uint  `gorm:"column:folder_id" json:"folder_id" form:"folder_id"`
	Summary      string `gorm:"column:summary" json:"summary" form:"summary"`
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

	db.AutoMigrate(&Contract{}, &Folder{})
	seedDefaultFolders()
}

func ensureUploadsDir() {
	// アップロード保存用のディレクトリを作成
	os.MkdirAll("uploads", 0755)
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

func setupRouter() *gin.Engine {
	r := gin.Default()

	// 追加：同じフォルダにある静的ファイル（HTMLなど）を使えるようにする
	r.StaticFile("/", "./index.html")
	r.StaticFile("/index.html", "./index.html")
	r.StaticFile("/contract_detail.html", "./contract_detail.html")
	r.Static("/uploads", "./uploads") // アップロードされたファイルへのアクセスを許可

	// ルーティング
	r.GET("/hello", helloHandler)
	r.GET("/contracts", getContractsHandler)
	r.GET("/contracts/:id", getContractDetailHandler)
	r.POST("/contracts", createContractHandler)
	r.DELETE("/contracts/:id", deleteContractHandler)
	r.PUT("/contracts/:id", updateContractHandler)
	r.POST("/api/analyze", analyzeContractHandler)
	r.POST("/contracts/:id/analyze", reanalyzeContractHandler)

	// フォルダ関連のルーティング
	r.GET("/folders", getFoldersHandler)
	r.POST("/folders", createFolderHandler)
	r.PUT("/folders/:id", updateFolderHandler)
	r.DELETE("/folders/:id", deleteFolderHandler)

	return r
}

// --- ハンドラー関数 ---

func helloHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"message": "Hello, Go World!"})
}

func getContractsHandler(c *gin.Context) {
	var contracts []Contract
	query := db.Model(&Contract{})

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
	if err := db.Delete(&Contract{}, id).Error; err != nil {
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