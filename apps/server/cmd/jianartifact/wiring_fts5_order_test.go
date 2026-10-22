package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestOpenServicesChecksFTS5BeforeMigrate 用静态解析钉住一处**顺序**约定：fts5/trigram 自检
// 必须排在 db.Migrate() 之前。
//
// 为什么用静态断言：这条顺序的效果只在「SQLite 构建缺 fts5 或 trigram」时才可观察，而测试
// 用的构建本身带这些能力，运行时构造不出该场景；可顺序一旦被改回去，缺能力的部署就会先撞上
// 迁移 0049 抛出的泛化 SQL 错误（它的第一条语句就是 CREATE VIRTUAL TABLE ... USING fts5），
// 正是这个自检要消灭的形态。静态解析能确定性地守住它，且失败信息直接给出两处调用的行号。
func TestOpenServicesChecksFTS5BeforeMigrate(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "wiring.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 wiring.go：%v", err)
	}

	var openServices *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "openServices" {
			openServices = fn
			break
		}
	}
	if openServices == nil {
		t.Fatal("wiring.go 里找不到 openServices")
	}

	checkPos, migratePos := token.NoPos, token.NoPos
	ast.Inspect(openServices, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "CheckFTS5Support":
			if !checkPos.IsValid() {
				checkPos = call.Pos()
			}
		case "Migrate":
			if !migratePos.IsValid() {
				migratePos = call.Pos()
			}
		}
		return true
	})
	if !checkPos.IsValid() {
		t.Fatal("openServices 没有调用 persistence.CheckFTS5Support()：缺启动自检时，" +
			"缺 fts5 的构建会在迁移阶段报泛化 SQL 错误")
	}
	if !migratePos.IsValid() {
		t.Fatal("openServices 没有调用 db.Migrate()")
	}
	if checkPos > migratePos {
		t.Fatalf("fts5 自检必须排在 Migrate 之前（自检在第 %d 行、迁移在第 %d 行）："+
			"迁移 0049 的第一条语句就是建 fts5 虚拟表，排在后面会让缺能力的部署先看到泛化 SQL 错误",
			fset.Position(checkPos).Line, fset.Position(migratePos).Line)
	}
}
