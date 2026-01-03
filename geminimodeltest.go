package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

func mainGeminiTest() {
	// あなたのAPIキーを入れてください
	apiKey := "AIzaSyAoThAN--5-SU86_Hyf2eCMcBSmTtvtS-g"
	url := "https://generativelanguage.googleapis.com/v1beta/models?key=" + apiKey

	resp, err := http.Get(url)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Fatalf("API Request failed with status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Fatal(err)
	}

	fmt.Println("--- 利用可能なモデル一覧 ---")
	for _, m := range result.Models {
		// モデル名を表示
		fmt.Printf("Model Name: %s\n", m.Name)
		// サポートされている機能も確認（generateContentがあるか）
		fmt.Printf("Supported Methods: %v\n\n", m.SupportedGenerationMethods)
	}
}