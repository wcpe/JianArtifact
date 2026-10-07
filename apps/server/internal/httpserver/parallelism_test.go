// 顶层用例并行执行的组织约定。
//
// httpserver 的用例绝大多数是「起服务 + 打若干请求」的 I/O 等待型，彼此不共享可变状态：
// 每个用例用自己的 t.TempDir()（独立 SQLite 库文件 + 独立 blob 目录）、自己的
// httptest.NewServer（随机端口）、自己的 handler / store / 领域服务实例；本包
// （含 httpserver 内部测试文件）没有任何包级可变变量，也没有固定端口、
// 固定路径、log.SetOutput、os.Chdir 之类的进程级写入。
//
// 因此除下列两类用例外，顶层用例一律声明 t.Parallel()：
//
//  1. 调用 t.Setenv 的用例。t.Setenv 写的是进程级环境变量，Go 明确禁止它与
//     t.Parallel 同时使用。顶层并行用例要等全部串行用例结束才会真正开跑，
//     所以串行用例里的 t.Setenv 不会与并行用例的请求重叠。
//     见 integration_test.go / oci_test.go / pypi_test.go / credential_ref_audit_test.go。
//  2. 判据读进程级全局计数器的用例（runtime.MemStats.TotalAlloc）。
//     见 pypi_test.go 的 TestPypiLegacyUploadStreamsLargeDistribution。
//
// 并发度由 -test.parallel 约束（默认为 GOMAXPROCS），因此并行化不会把上百个
// 测试环境同时压进内存：在 4 核 CI 上同时运行的环境不超过 4 个。
package httpserver_test
