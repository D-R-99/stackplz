/*
Copyright © 2022 NAME HERE <EMAIL ADDRESS>

*/
package cmd

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "io/ioutil"
    "log"
    "os"
    "os/signal"
    "path"
    "stackplz/assets"
    "stackplz/user/config"
    "stackplz/user/event"
    "stackplz/user/event_parser"
    "stackplz/user/module"
    "stackplz/user/rpc"
    "stackplz/user/util"
    "strconv"
    "strings"
    "sync"
    "syscall"

    "github.com/spf13/cobra"
    "golang.org/x/exp/slices"
)

var Logger *log.Logger

func NewLogger(log_path string) *log.Logger {
    if Logger != nil {
        return Logger
    }
    // 首先根据全局设定设置日志输出
    Logger = log.New(os.Stdout, "", 0)
    if gconfig.LogFile != "" {
        _, err := os.Stat(log_path)
        if err != nil {
            if os.IsNotExist(err) {
                os.Remove(log_path)
            }
        }
        f, err := os.Create(log_path)
        if err != nil {
            Logger.Fatal(err)
            os.Exit(1)
        }
        if gconfig.Quiet {
            // 直接设置 则不会输出到终端
            Logger.SetOutput(f)
        } else {
            // 这样可以同时输出到终端
            mw := io.MultiWriter(os.Stdout, f)
            Logger.SetOutput(mw)
        }
    }

    return Logger
}

var gconfig = config.NewGlobalConfig()
var mconfig = config.NewModuleConfig()

var rootCmd = &cobra.Command{
    Use:               "stackplz",
    Short:             "打印堆栈信息，目前仅支持5.10+内核，出现崩溃请升级系统版本",
    Long:              "基于eBPF的堆栈追踪工具，指定目标程序的uid、库文件路径和符号即可\n\t./stackplz --name com.sfx.ebpf --syscall openat -o tmp.log --debug",
    PersistentPreRunE: persistentPreRunEFunc,
    Run:               runFunc,
}

// cobra.Command 中几个函数执行的顺序
// PersistentPreRun
// PreRun
// Run
// PostRun
// PersistentPostRun

func persistentPreRunEFunc(command *cobra.Command, args []string) error {
    // 在执行子命令的时候 上级命令的 PersistentPreRun/PersistentPreRunE 会先执行

    var err error

    // 首先根据全局设定设置日志输出
    dir, _ := os.Getwd()
    log_path := dir + "/" + gconfig.LogFile
    if gconfig.LogFile != "" {
        _, err := os.Stat(log_path)
        if err != nil {
            if os.IsNotExist(err) {
                os.Remove(log_path)
            } else {
                fmt.Printf("stat %s failed, error:%v", log_path, err)
                os.Exit(1)
            }
        } else {
            os.Remove(log_path)
        }
    }

    // 在 init 之后各个选项的 flag 还没有初始化 到这里才初始化 所以在这里最先设置好 logger
    logger := NewLogger(log_path)
    mconfig.SetLogger(logger)
    if !gconfig.NoCheck {
        // 先检查必要的配置
        err = util.CheckKernelConfig()
        if err != nil {
            logger.Fatalf("CheckKernelConfig failed, error:%v", err)
        }
    }
    if gconfig.Btf {
        mconfig.ExternalBTF = ""
    } else {
        if !util.HasEnableBTF {
            // 检查平台 判断是不是开发板
            gconfig.ExternalBTF = findBTFAssets()
            mconfig.ExternalBTF = gconfig.ExternalBTF
        } else {
            mconfig.ExternalBTF = ""
        }
    }
    // 这个操作有点耗时 内核版本符合要求的用不着检查 先不要这个操作
    // 检查符号情况 用于判断部分选项是否能启用
    // has_bpf_probe_read_user, err := findKallsymsSymbol("bpf_probe_read_user")
    // if err != nil {
    //     logger.Printf("bpf_probe_read_user err:%v", err)
    //     return err
    // }
    // if !has_bpf_probe_read_user {
    //     logger.Printf("!!! may not support for this machine, has no bpf_probe_read_user")
    // }

    // 第一步先释放用于获取堆栈信息的外部库
    exec_path, err := os.Executable()
    if err != nil {
        return fmt.Errorf("please build as executable binary, %v", err)
    }
    if gconfig.Debug {
        logger.Printf("Executable:%s", exec_path)
    }
    // 获取一次 后面用得到 免去重复获取
    exec_path = path.Dir(exec_path)
    gconfig.ExecPath = exec_path
    _, err = os.Stat(exec_path + "/" + "preload_libs")
    var has_restore bool = false
    if err != nil {
        if os.IsNotExist(err) {
            // 路径不存在就自动释放
            err = assets.RestoreAssets(exec_path, "preload_libs")
            if err != nil {
                return fmt.Errorf("RestoreAssets preload_libs failed, %v", err)
            }
            has_restore = true
        } else {
            // 未知异常 比如权限问题 那么直接结束
            return err
        }
    }
    if gconfig.Prepare {
        // 认为是需要重新释放一次
        if !has_restore {
            err = assets.RestoreAssets(exec_path, "preload_libs")
            if err != nil {
                return fmt.Errorf("RestoreAssets preload_libs failed, %v", err)
            }
        }
        fmt.Println("RestoreAssets preload_libs success")
        os.Exit(0)
    }

    if gconfig.Rpc {
        fmt.Printf("rpc mode, listen path:%s\n", gconfig.RpcPath)
        return nil
    }

    mconfig.Parse_Idlist("UidWhitelist", gconfig.Uid)
    mconfig.Parse_Idlist("UidBlacklist", gconfig.NoUid)
    mconfig.Parse_Idlist("PidWhitelist", gconfig.Pid)
    mconfig.Parse_Idlist("PidBlacklist", gconfig.NoPid)
    mconfig.Parse_Idlist("TidWhitelist", gconfig.Tid)
    mconfig.Parse_Idlist("TidBlacklist", gconfig.NoTid)
    mconfig.Parse_Namelist("TNameWhitelist", gconfig.TName)
    mconfig.Parse_Namelist("TNameBlacklist", gconfig.NoTName)

    mconfig.Parse_ArgFilter(gconfig.ArgFilter)

    pis := util.Get_PackageInfos()
    // 根据 pid 解析进程架构、获取库文件搜索路径
    for _, process_pid := range mconfig.PidWhitelist {
        process_uid := pis.FindUidByPid(process_pid)
        is_find, info := pis.FindPackageByUid(process_uid)
        if !is_find {
            logger.Printf("can not find package for process_pid=%d", process_pid)
            continue
        }
        addLibPath(info.Name)
        // 解析maps 将maps中的路径加入搜索路径中
        search_paths, err := event.FindLibPaths(process_pid)
        if err != nil {
            return err
        }
        for _, search_path := range search_paths {
            if !slices.Contains(gconfig.LibraryDirs, search_path) {
                gconfig.LibraryDirs = append(gconfig.LibraryDirs, search_path)
            }
        }
    }
    // 根据 uid 解析进程架构、获取库文件搜索路径
    for _, pkg_uid := range mconfig.UidWhitelist {
        if pkg_uid == 0 || pkg_uid == 1000 || pkg_uid == 2000 {
            continue
        }
        is_find, info := pis.FindPackageByUid(pkg_uid)
        if !is_find {
            return fmt.Errorf("can not find pkg_uid=%d", pkg_uid)
        }
        addLibPath(info.Name)
    }
    // 根据 pkg_name 解析进程架构、获取库文件搜索路径
    pkg_names := strings.Split(gconfig.Name, ",")
    for _, pkg_name := range pkg_names {
        switch pkg_name {
        case "":
        case "root":
            mconfig.TraceGroup |= util.GROUP_ROOT
        case "system":
            mconfig.TraceGroup |= util.GROUP_SYSTEM
        case "shell":
            mconfig.TraceGroup |= util.GROUP_SHELL
        case "app":
            mconfig.TraceGroup |= util.GROUP_APP
        case "iso":
            mconfig.TraceGroup |= util.GROUP_ISO
        default:
            is_find, info := pis.FindPackageByName(pkg_name)
            if !is_find {
                return fmt.Errorf("can not find pkg_name=%s", pkg_name)
            }
            // 根据包名查找进程名 再获取库搜索路径
            pid_list := FindPidByName(pkg_name)
            for _, pkg_pid := range pid_list {
                // 解析maps 将maps中的路径加入搜索路径中
                search_paths, err := event.FindLibPaths(pkg_pid)
                if err != nil {
                    return err
                }
                for _, search_path := range search_paths {
                    if !slices.Contains(gconfig.LibraryDirs, search_path) {
                        gconfig.LibraryDirs = append(gconfig.LibraryDirs, search_path)
                    }
                }
            }
            // 对于system进程不添加uid
            mconfig.PidWhitelist = append(mconfig.PidWhitelist, pid_list...)
            if info.Uid != 1000 {
                mconfig.UidWhitelist = append(mconfig.UidWhitelist, info.Uid)
            }
            addLibPath(pkg_name)
            mconfig.PkgNamelist = append(mconfig.PkgNamelist, pkg_name)
        }
    }
    // 后面更新map的时候不影响 列表不去重也行

    mconfig.InitCommonConfig(gconfig)

    // 1. hook uprobe
    if len(gconfig.HookPoint) > 0 {
        err = gconfig.Parse_Libinfo(gconfig.Library, mconfig.StackUprobeConf)
        if err != nil {
            return err
        }
        err = mconfig.StackUprobeConf.Parse_HookPoint(gconfig.HookPoint)
        if err != nil {
            return err
        }
        u_syscall := mconfig.StackUprobeConf.GetSyscall()
        if u_syscall != "" {
            gconfig.SysCall = u_syscall
        }
    }

    // 3. hook config
    if len(gconfig.ConfigFiles) > 0 {
        mconfig.LoadConfig(gconfig)
    }

    // 2. hook syscall
    mconfig.SysCallConf.Parse_SysWhitelist(gconfig)
    mconfig.SysCallConf.Parse_SysBlacklist(gconfig.NoSysCall)

    // 4. watch breakpoint
    var brk_base uint64 = 0x0
    if gconfig.BrkLib != "" {
        if gconfig.BrkPid <= 0 {
            return errors.New("must set --brk-pid when use --brk-lib option")
        }
        lib_info, err := event.FindLibInMaps(uint32(gconfig.BrkPid), gconfig.BrkLib)
        if err != nil {
            return err
        }
        brk_base = lib_info.BaseAddr
    }

    if gconfig.BrkAddr != "" && strings.HasPrefix(gconfig.BrkAddr, "0x") {
        if gconfig.BrkLen <= 0 && gconfig.BrkLen > 8 {
            return errors.New(fmt.Sprintf("BrkLen %d invaild, support [1, 8]", gconfig.BrkLen))
        }
        mconfig.BrkLen = gconfig.BrkLen
        mconfig.BrkPid = gconfig.BrkPid
        infos := strings.Split(gconfig.BrkAddr, ":")
        if len(infos) > 2 {
            return errors.New(fmt.Sprintf("parse for %s failed, format invaild", gconfig.BrkAddr))
        }
        if len(infos) == 2 {
            if infos[1] == "r" {
                mconfig.BrkType = util.HW_BREAKPOINT_R
            } else if infos[1] == "w" {
                mconfig.BrkType = util.HW_BREAKPOINT_W
            } else if infos[1] == "x" {
                mconfig.BrkType = util.HW_BREAKPOINT_X
            } else if infos[1] == "rw" {
                mconfig.BrkType = util.HW_BREAKPOINT_RW
            } else {
                return errors.New(fmt.Sprintf("parse BrkType for %s failed, choose:r,w,x,rw", infos[1]))
            }
        } else {
            mconfig.BrkType = util.HW_BREAKPOINT_X
        }
        addr, err := strconv.ParseUint(strings.TrimPrefix(infos[0], "0x"), 16, 64)
        if err != nil {
            return errors.New(fmt.Sprintf("parse for %s failed, err:%v", gconfig.BrkAddr, err))
        }
        mconfig.BrkAddr = brk_base + addr
    }

    // 检查hook设定
    enable_hook := false
    if len(mconfig.StackUprobeConf.Points) > 0 {
        if gconfig.DumpRet {
            DumpSymbolRet()
        }
        enable_hook = true
        logger.Printf("hook uprobe, count:%d", len(mconfig.StackUprobeConf.Points))
    }
    if mconfig.SysCallConf.Enable {
        enable_hook = true
        logger.Printf("hook syscall count:%d", len(mconfig.SysCallConf.SysWhitelist))
    }
    if mconfig.BrkAddr > 0 {
        enable_hook = true
        if mconfig.BrkAddr&0xffffff0000000000 > 0 {
            mconfig.BrkKernel = true
        } else {
            mconfig.BrkKernel = false
        }
        logger.Printf("set breakpoint at kernel:%t, addr:0x%x", mconfig.BrkKernel, mconfig.BrkAddr)
    }
    if !enable_hook {
        logger.Fatal("hook nothing, plz set -w/--point or -s/--syscall or --brk")
    }
    if gconfig.ParseFile != "" {
        parser := event_parser.NewEventParser()
        parser.SetLogger(logger)
        parser.SetConf(mconfig)
        parser.ParseDump(gconfig.ParseFile)
    }
    mconfig.DumpOpen(gconfig.DumpFile)
    return nil
}

func runFunc(command *cobra.Command, args []string) {
    stopper := make(chan os.Signal, 1)
    signal.Notify(stopper, os.Interrupt, syscall.SIGTERM)
    ctx, cancelFun := context.WithCancel(context.TODO())
    if gconfig.Rpc {
        rpc.SetupRpc(ctx, Logger, gconfig)
        rpc.StartRpcServer(stopper, gconfig.RpcPath)
        os.Exit(0)
    }
    var runMods uint8
    var runModules = make(map[string]module.IModule)
    var wg sync.WaitGroup

    var modNames []string
    if mconfig.BrkAddr != 0 {
        modNames = append(modNames, module.MODULE_NAME_BRK)
    } else if mconfig.SysCallConf.Enable {
        modNames = append(modNames, module.MODULE_NAME_PERF)
        modNames = append(modNames, module.MODULE_NAME_SYSCALL)
    } else if len(mconfig.StackUprobeConf.Points) > 0 {
        modNames = append(modNames, module.MODULE_NAME_PERF)
        modNames = append(modNames, module.MODULE_NAME_STACK)
    } else {
        Logger.Fatal("hook nothing, plz set -w/--point or -s/--syscall or --brk")
    }
    for _, modName := range modNames {
        // 现在合并成只有一个模块了 所以直接通过名字获取
        mod := module.GetModuleByName(modName)

        mod.Init(ctx, Logger, mconfig)
        err := mod.Run()
        if err != nil {
            Logger.Printf("%s\tmodule Run failed, [skip it]. error:%+v", mod.Name(), err)
            os.Exit(1)
        }
        runModules[mod.Name()] = mod
        if gconfig.Debug {
            Logger.Printf("%s\tmodule started successfully", mod.Name())
        }
        wg.Add(1)
        runMods++

    }
    if runMods > 0 {
        Logger.Printf("start %d modules", runMods)
        go func() {
            scanner := bufio.NewScanner(os.Stdin)
            for {
                scanner.Scan()
                err := scanner.Err()
                if err != nil {
                    Logger.Printf("get input from console failed, err:%v", err)
                    syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
                }
                input_text := scanner.Text()
                if input_text == "c" {
                    event.LetItRun()
                }
            }
        }()

        <-stopper
    } else {
        Logger.Println("No runnable modules, Exit(1)")
        os.Exit(1)
    }
    cancelFun()

    for _, mod := range runModules {
        err := mod.Close()
        Logger.Println("mod Close")
        wg.Done()
        if err != nil {
            Logger.Fatalf("%s:module close failed. error:%+v", mod.Name(), err)
        }
    }
    wg.Wait()
    // 关闭打开的dump文件
    mconfig.DumpClose()
    os.Exit(0)
}

func addLibPath(name string) {
    content, err := util.RunCommand("pm", "path", name)
    if err != nil {
        panic(err)
    }
    for _, line := range strings.Split(content, "\n") {
        parts := strings.Split(line, ":")
        if len(parts) == 2 {
            // 将 apk 文件也作为搜索路径
            apk_path := parts[1]
            _, err := os.Stat(apk_path)
            if err == nil {
                if !slices.Contains(gconfig.LibraryDirs, apk_path) {
                    if gconfig.Debug {
                        mconfig.GetLogger().Printf("add lib_search_path => [%s]", apk_path)
                    }
                    gconfig.LibraryDirs = append(gconfig.LibraryDirs, apk_path)
                }
            }
            // 将 apk + /lib/arm64 作为搜索路径
            items := strings.Split(parts[1], "/")
            lib_search_path := strings.Join(items[:len(items)-1], "/") + "/lib/arm64"
            _, err = os.Stat(lib_search_path)
            if err == nil {
                if !slices.Contains(gconfig.LibraryDirs, lib_search_path) {
                    if gconfig.Debug {
                        mconfig.GetLogger().Printf("add lib_search_path => [%s]", lib_search_path)
                    }
                    gconfig.LibraryDirs = append(gconfig.LibraryDirs, lib_search_path)
                }
            }
        }
    }
}

func findBTFAssets() string {
    var utsname syscall.Utsname
    err := syscall.Uname(&utsname)
    if err != nil {
        fmt.Println("Error:", err)
        os.Exit(1)
    }
    btf_file := "p4_min.btf"
    if strings.Contains(util.B2S(utsname.Release[:]), "rockchip") {
        btf_file = "rock5b-5.10-arm64_min.btf"
    }
    Logger.Printf("findBTFAssets btf_file=%s", btf_file)
    return btf_file
}

func findKallsymsSymbol(symbol string) (bool, error) {
    find := false
    content, err := ioutil.ReadFile("/proc/kallsyms")
    if err != nil {
        return find, fmt.Errorf("Error when opening file:%v", err)
    }
    lines := string(content)
    for _, line := range strings.Split(lines, "\n") {
        parts := strings.SplitN(line, " ", 3)
        if len(parts) != 3 {
            continue
        }
        if parts[2] == symbol {
            find = true
            break
        }
    }
    return find, nil
}

func DumpSymbolRet() {
    for _, point := range mconfig.StackUprobeConf.Points {
        if point.Symbol == "" {
            continue
        }
        content, err := util.RunCommand("sh", "-c", fmt.Sprintf("readelf -s %s | grep %s", point.LibPath, point.Symbol))
        if err != nil {
            Logger.Printf("warn, readelf parse for %s failed, lib:%s", point.Symbol, point.LibPath)
            continue
        }
        var (
            sym_num   uint32
            sym_value int64
            sym_size  int64
            sym_type  string
            sym_bind  string
            sym_vis   string
            sym_ndx   string
            sym_name  string
        )
        lines := strings.Split(content, "\n")
        for _, line := range lines {
            line = strings.TrimSpace(line)
            if line == "" {
                continue
            }
            reader := strings.NewReader(line)
            n, err := fmt.Fscanf(reader, "%d: %x %d %s %s %s %s %s", &sym_num, &sym_value, &sym_size, &sym_type, &sym_bind, &sym_vis, &sym_ndx, &sym_name)
            if err == nil && n == 8 {
                ret_info, err := util.FindRet(point.LibPath, sym_value, sym_size)
                if err != nil || ret_info == "" {
                    Logger.Printf("FindRet for %s failed, sym:%s offset:0x%x", point.Symbol, sym_name, sym_value)
                    continue
                } else {
                    Logger.Printf("FindRet for %s -> [%s] sym:%s offset:0x%x", point.Symbol, ret_info, sym_name, sym_value)
                }
            }
        }
    }
    os.Exit(0)
}

func FindPidByName(name string) []uint32 {
    var pid_list []uint32
    content, err := util.RunCommand("sh", "-c", "ps -ef -o name,pid,ppid | grep ^"+name)
    if err != nil {
        Logger.Printf("warn, no running process of %s", name)
        return pid_list
    }
    lines := strings.Split(content, "\n")
    for _, line := range lines {
        parts := strings.Fields(line)
        value, err := strconv.ParseUint(parts[1], 10, 32)
        if err != nil {
            panic(err)
        }
        pid_list = append(pid_list, uint32(value))
    }
    return pid_list
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
    // 异常时不显示帮助信息 只提示异常 因为帮助信息占据的版面太多
    rootCmd.SilenceUsage = true
    rootCmd.CompletionOptions.DisableDefaultCmd = true
    err := rootCmd.Execute()
    if err != nil {
        os.Exit(1)
    }
}

func init() {
    cobra.EnablePrefixMatching = false
    // 考虑到外部库更新 每个版本首次运行前 都应该执行一次
    rootCmd.PersistentFlags().BoolVar(&gconfig.Prepare, "prepare", false, "prepare libs")
    // 过滤设定
    rootCmd.PersistentFlags().StringVarP(&gconfig.Name, "name", "n", "", "must set uid or package name")

    rootCmd.PersistentFlags().StringVarP(&gconfig.Uid, "uid", "u", "", "uid white list")
    rootCmd.PersistentFlags().StringVarP(&gconfig.Pid, "pid", "p", "", "pid white list")
    rootCmd.PersistentFlags().StringVarP(&gconfig.Tid, "tid", "t", "", "tid white list")
    rootCmd.PersistentFlags().StringVar(&gconfig.NoUid, "no-uid", "", "uid black list")
    rootCmd.PersistentFlags().StringVar(&gconfig.NoPid, "no-pid", "", "pid black list")
    rootCmd.PersistentFlags().StringVar(&gconfig.NoTid, "no-tid", "", "tid black list")

    rootCmd.PersistentFlags().StringVar(&gconfig.TName, "tname", "", "thread name white list")
    rootCmd.PersistentFlags().StringVar(&gconfig.NoTName, "no-tname", "", "thread name black list")
    rootCmd.PersistentFlags().StringArrayVarP(&gconfig.ArgFilter, "filter", "f", []string{}, "arg filter rule")

    rootCmd.PersistentFlags().StringVar(&gconfig.KillSignal, "kill", "", "send signal when hit uprobe hook, e.g. SIGSTOP/SIGABRT/SIGTRAP/...")
    rootCmd.PersistentFlags().StringVar(&gconfig.TKillSignal, "tkill", "", "send signal to thread when hit uprobe hook, e.g. SIGSTOP/SIGABRT/SIGTRAP/...")
    rootCmd.PersistentFlags().BoolVar(&gconfig.Rpc, "rpc", false, "enable rpc")
    rootCmd.PersistentFlags().StringVar(&gconfig.RpcPath, "rpc-path", "127.0.0.1:41718", "rpc path, default 127.0.0.1:41718")
    // 硬件断点设定
    rootCmd.PersistentFlags().StringVar(&gconfig.BrkAddr, "brk", "", "set hardware breakpoint address")
    rootCmd.PersistentFlags().IntVar(&gconfig.BrkPid, "brk-pid", -1, "set hardware breakpoint pid")
    rootCmd.PersistentFlags().StringVar(&gconfig.BrkLib, "brk-lib", "", "as library base address")
    rootCmd.PersistentFlags().Uint64Var(&gconfig.BrkLen, "brk-len", 4, "hardware breakpoint length, default 4, support [1, 8]")
    // 缓冲区大小设定 单位M
    rootCmd.PersistentFlags().Uint32VarP(&gconfig.Buffer, "buffer", "b", 8, "perf cache buffer size, default 8M")
    rootCmd.PersistentFlags().Uint32Var(&gconfig.MaxOp, "maxop", 64, "max operation count for uprobe, at least 192 for string array")
    // 堆栈输出设定
    rootCmd.PersistentFlags().BoolVar(&gconfig.ManualStack, "mstack", false, "manual parse stack")
    rootCmd.PersistentFlags().BoolVar(&gconfig.UnwindStack, "stack", false, "enable unwindstack")
    rootCmd.PersistentFlags().Uint32VarP(&gconfig.StackSize, "stack-size", "", 8192, "stack dump size, default 8192 bytes, max 65528 bytes")
    rootCmd.PersistentFlags().BoolVar(&gconfig.ShowRegs, "regs", false, "show regs")
    rootCmd.PersistentFlags().BoolVar(&gconfig.GetOff, "getoff", false, "try get pc and lr offset")
    // 日志设定
    rootCmd.PersistentFlags().BoolVarP(&gconfig.Debug, "debug", "d", false, "enable debug logging")
    rootCmd.PersistentFlags().BoolVarP(&gconfig.Quiet, "quiet", "q", false, "wont logging to terminal when used")
    rootCmd.PersistentFlags().BoolVar(&gconfig.Color, "color", false, "enable color for log file")
    rootCmd.PersistentFlags().BoolVarP(&gconfig.FmtJson, "json", "j", false, "log event as json format")
    rootCmd.PersistentFlags().StringVarP(&gconfig.LogFile, "out", "o", "stackplz_tmp.log", "save the log to file")
    // 适合收集大量数据 减少数据丢失
    rootCmd.PersistentFlags().StringVar(&gconfig.DumpFile, "dump", "", "save perf data to file")
    rootCmd.PersistentFlags().StringVar(&gconfig.ParseFile, "parse", "", "parse perf data as json or readable format")
    // 常规ELF库hook设定
    rootCmd.PersistentFlags().StringVarP(&gconfig.Library, "lib", "l", "libc.so", "lib name or lib full path, default is libc.so")
    rootCmd.PersistentFlags().StringArrayVarP(&gconfig.HookPoint, "point", "w", []string{}, "hook point config, e.g. strstr+0x0[str,str] write[int,buf:128,int]")
    rootCmd.PersistentFlags().StringVar(&gconfig.RegName, "reg", "", "get the offset of reg")
    rootCmd.PersistentFlags().BoolVarP(&gconfig.DumpRet, "dumpret", "", false, "dump ret offset for symbol")
    rootCmd.PersistentFlags().BoolVarP(&gconfig.DumpHex, "dumphex", "", false, "dump buffer as hex")
    rootCmd.PersistentFlags().BoolVarP(&gconfig.ShowPC, "showpc", "", false, "show origin pc register value")
    rootCmd.PersistentFlags().BoolVarP(&gconfig.ShowTime, "showtime", "", false, "show event boot time info")
    rootCmd.PersistentFlags().BoolVarP(&gconfig.ShowUid, "showuid", "", false, "show process uid info")
    rootCmd.PersistentFlags().BoolVarP(&gconfig.NoCheck, "nocheck", "", false, "disable check for bpf")
    rootCmd.PersistentFlags().BoolVarP(&gconfig.Btf, "btf", "", false, "declare BTF enabled")
    // syscall hook
    rootCmd.PersistentFlags().StringVarP(&gconfig.SysCall, "syscall", "s", "", "filter syscalls")
    rootCmd.PersistentFlags().StringVar(&gconfig.NoSysCall, "no-syscall", "", "syscall black list, max 20")
    // config hook
    rootCmd.PersistentFlags().StringArrayVarP(&gconfig.ConfigFiles, "config", "c", []string{}, "hook config file")
}
