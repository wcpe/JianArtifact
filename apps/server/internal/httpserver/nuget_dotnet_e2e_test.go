package httpserver_test

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNuGetDotnetPushRestoreAndBuildAcrossHostedProxyAndGroup(t *testing.T) {
	if _, err := exec.LookPath("dotnet"); err != nil {
		t.Skip("本机未安装 dotnet，跳过 NuGet 原生客户端验收")
	}
	e := newProtocolEnv(t)
	server := httptest.NewServer(e.h)
	t.Cleanup(server.Close)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "nuget-upstream", "nuget", "hosted")
	proxyConfig, err := json.Marshal(map[string]string{"remoteUrl": server.URL + "/nuget/nuget-upstream/v3/index.json"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.repoRepo.Create("nuget-proxy-e2e", "nuget", "proxy", "public", string(proxyConfig)); err != nil {
		t.Fatalf("创建 NuGet proxy：%v", err)
	}
	groupConfig, err := json.Marshal(map[string]any{"members": []string{"nuget-proxy-e2e"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.repoRepo.Create("nuget-group-e2e", "nuget", "group", "public", string(groupConfig)); err != nil {
		t.Fatalf("创建 NuGet group：%v", err)
	}

	for _, tc := range []struct {
		name    string
		id      string
		version string
		repo    string
	}{
		{name: "hosted", id: "Hosted.E2E.Package", version: "1.0.0", repo: "nuget-upstream"},
		{name: "proxy", id: "Proxy.E2E.Package", version: "1.0.0", repo: "nuget-proxy-e2e"},
		{name: "group", id: "Group.E2E.Package", version: "1.0.0", repo: "nuget-group-e2e"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			packagePath := buildDotnetNuGetPackage(t, tc.id, tc.version)
			pushDotnetNuGetPackage(t, packagePath, server.URL+"/nuget/nuget-upstream/api/v2/package", admin)
			restoreAndBuildDotnetConsumer(t, server.URL+"/nuget/"+tc.repo+"/v3/index.json", tc.id, tc.version)
		})
	}
}

func buildDotnetNuGetPackage(t *testing.T, id, version string) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "package")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(directory, "Package.csproj")
	contents := `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>net9.0</TargetFramework><PackageId>` + id + `</PackageId><Version>` + version + `</Version></PropertyGroup></Project>`
	if err := os.WriteFile(project, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "Value.cs"), []byte("namespace E2E; public static class Value { public static int Get() => 1; }"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "packages")
	runDotnet(t, directory, "pack", project, "--output", output, "--nologo")
	return filepath.Join(output, strings.ToLower(id)+"."+version+".nupkg")
}

func pushDotnetNuGetPackage(t *testing.T, packagePath, source, apiKey string) {
	t.Helper()
	directory := filepath.Dir(packagePath)
	config := writeNuGetConfig(t, directory, source)
	runDotnet(t, directory, "nuget", "push", packagePath, "--source", "jian", "--api-key", apiKey, "--no-service-endpoint", "--configfile", config)
}

func restoreAndBuildDotnetConsumer(t *testing.T, source, id, version string) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "consumer")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(directory, "Consumer.csproj")
	contents := `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><OutputType>Exe</OutputType><TargetFramework>net9.0</TargetFramework><RestoreSources>` + source + `</RestoreSources></PropertyGroup><ItemGroup><PackageReference Include="` + id + `" Version="` + version + `" /></ItemGroup></Project>`
	if err := os.WriteFile(project, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "Program.cs"), []byte("using System; Console.WriteLine(\"ok\");"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := writeNuGetConfig(t, directory, source)
	runDotnet(t, directory, "restore", project, "--disable-parallel", "--nologo", "--configfile", config)
	runDotnet(t, directory, "build", project, "--no-restore", "--nologo")
}

func writeNuGetConfig(t *testing.T, directory, source string) string {
	t.Helper()
	path := filepath.Join(directory, "NuGet.Config")
	contents := `<?xml version="1.0" encoding="utf-8"?><configuration><packageSources><clear /><add key="jian" value="` + source + `" allowInsecureConnections="true" /></packageSources></configuration>`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runDotnet(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("dotnet", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "NUGET_PACKAGES="+filepath.Join(t.TempDir(), "nuget-cache"), "DOTNET_CLI_TELEMETRY_OPTOUT=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("dotnet 命令失败：%s", output)
	}
}
