const express = require("express");
const multer = require("multer"); // ファイルアップロード用ライブラリ
const path = require("path");
const fs = require("fs");

const app = express();
const PORT = 3000;

// 静的ファイル（index.htmlなど）を現在のディレクトリから配信
app.use(express.static(__dirname));

// JSONボディのパース用（通常のAPI用）
app.use(express.json());

// --- Multerの設定（ファイル保存先とファイル名） ---
const storage = multer.diskStorage({
  destination: function (req, file, cb) {
    const uploadDir = "uploads/";
    // uploadsフォルダがなければ作成する
    if (!fs.existsSync(uploadDir)) {
      fs.mkdirSync(uploadDir);
    }
    cb(null, uploadDir);
  },
  filename: function (req, file, cb) {
    // ファイル名が重複しないように現在時刻を付与 (例: 1672531200000-contract.pdf)
    cb(null, Date.now() + "-" + file.originalname);
  },
});

const upload = multer({ storage: storage });
// ---------------------------------------------------

// データ保存用（簡易的なメモリ内データベース）
const DATA_FILE = path.join(__dirname, "data.json");
let contracts = [];
let currentId = 1;

// 起動時にデータを読み込む
if (fs.existsSync(DATA_FILE)) {
  try {
    const data = fs.readFileSync(DATA_FILE, "utf8");
    contracts = JSON.parse(data);
    if (contracts.length > 0) {
      currentId = Math.max(...contracts.map((c) => c.id)) + 1;
    }
  } catch (e) {
    console.error("Data load error:", e);
  }
}

// データを保存する関数
const saveData = () => {
  fs.writeFileSync(DATA_FILE, JSON.stringify(contracts, null, 2));
};

// 契約一覧取得 API
app.get("/contracts", (req, res) => {
  const { title, client_name } = req.query;
  let results = contracts;

  if (title) {
    results = results.filter((c) => c.title && c.title.includes(title));
  }
  if (client_name) {
    results = results.filter(
      (c) => c.client_name && c.client_name.includes(client_name)
    );
  }

  res.json(results);
});

// 契約登録 API
// upload.single('pdf') はフロントエンドの formData.append('pdf', ...) と名前を合わせます
app.post("/contracts", upload.single("pdf"), (req, res) => {
  // テキストデータは req.body に入ります
  const {
    title,
    client_name,
    amount,
    contract_date,
    start_date,
    end_date,
    auto_renewal,
    summary,
    folder_id,
  } = req.body;

  // ファイルデータは req.file に入ります（未選択の場合は undefined）
  const file = req.file;

  const newContract = {
    id: currentId++,
    title,
    client_name,
    amount: amount ? Number(amount) : 0,
    contract_date,
    start_date,
    end_date,
    auto_renewal: auto_renewal === "true", // FormDataでは文字列で送られるため
    summary,
    folder_id: folder_id ? Number(folder_id) : null,
    pdf_path: file ? file.path.replace(/\\/g, "/") : null, // Windowsパス対策: バックスラッシュをスラッシュに置換
    pdf_filename: file ? file.originalname : null, // 元のファイル名
    created_at: new Date(),
  };

  contracts.push(newContract);
  saveData();
  res.status(201).json(newContract);
});

// AI解析 API (モック)
app.post("/api/analyze", upload.single("pdf"), (req, res) => {
  const file = req.file;
  // 本来はここでPDFの内容を読み取り、AI API等で解析を行います
  // 現在はモックとしてダミーデータを返却します
  setTimeout(() => {
    res.json({
      title: file ? path.parse(file.originalname).name : "解析された契約書",
      client_name: "株式会社サンプル",
      amount: 1000000,
      contract_date: new Date().toISOString().split("T")[0],
      start_date: new Date().toISOString().split("T")[0],
      end_date: new Date(Date.now() + 31536000000).toISOString().split("T")[0], // 1年後
      auto_renewal: true,
      summary:
        "【AI解析結果】\nこれはサーバー側で生成されたモックの要約です。\n実際のPDF解析を行うには、サーバーサイドでOpenAI APIなどを呼び出す処理を実装する必要があります。\n\n主な契約内容:\n- 業務委託契約\n- 秘密保持条項あり",
    });
  }, 1000);
});

// 契約更新 API (PUT)
app.put("/contracts/:id", (req, res) => {
  const id = Number(req.params.id);
  const index = contracts.findIndex((c) => c.id === id);
  if (index === -1) {
    return res.status(404).json({ error: "Contract not found" });
  }

  // 既存データとマージ (部分更新対応)
  contracts[index] = {
    ...contracts[index],
    ...req.body,
  };

  saveData();
  res.json(contracts[index]);
});

// 契約削除 API
app.delete("/contracts/:id", (req, res) => {
  const id = Number(req.params.id);
  contracts = contracts.filter((c) => c.id !== id);
  saveData();
  res.status(204).send();
});

app.listen(PORT, () => {
  console.log(`Server is running on http://localhost:${PORT}`);
});
