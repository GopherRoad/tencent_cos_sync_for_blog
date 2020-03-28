
// 文件同步思路：
// 1. 旧仓库文件，读取文件列表，获取文件绝对路径缓存到字典数据结构中，路径作为 key，vaule 是文件的 HASH 值
// 2. 新仓库文件，读取文件列表，获取文件绝对路径缓存到字典数据结构中，路径作为 key，vaule 是文件的 HASH 值
// 备注：1 和 2 步骤可以并行进行，两个线程工作
// 3. 如果新仓库中的文件在旧仓库中不存在，那么添加新仓库文件路径到上传列表中；
//    如果新仓库中的文件在旧仓库中存在，但 HASH 值不同，那么也添加到上传列表中；
//    移除旧仓库中相对新仓库多出来的文件，新仓库没有，但旧仓库中有的文件。
// 4. 上传“上传”列表中的文件到腾讯 COS
// TODO:
// 1. 将路径描述符 ‘\’ 替换为 ‘/’，否则无法在 COS 端创建目录【完成】
// 2. 添加文件删除接口，删除新版不存在的旧文件【完成】
// 3. 给出命令行参数列表【完成】
// 4. 增加排除目录逻辑【完成】
// 5. 遗留问题：COS 上的空目录不能删除
package main

import (
    "context"
    "net/url"
    "os"
    "time"
    "strings"
    "path/filepath"

    "net/http"

    "fmt"
    "crypto/md5"
    "io"
    "bufio"

    cos "github.com/tencentyun/cos-go-sdk-v5"
    "github.com/tencentyun/cos-go-sdk-v5/debug"
)

var client *cos.Client

var default_secretid    string
var default_secretkey   string
var default_bucket_name string
var default_region      string
var base_old_repo_path  string
var base_new_repo_path  string

var old_repo_dir string
var new_repo_dir string

func md5sum(file string) string {
    f, err := os.Open(file)
    if err != nil { return "" }
    defer f.Close()

    r := bufio.NewReader(f)
    h := md5.New()

    _, err = io.Copy(h, r)
    if err != nil { return "" }

    return fmt.Sprintf("%x", h.Sum(nil))
}

// 遍历目录，找到所有文件，并计算每个文件的 hash，汇总成字典数据结构，最后通过 ch 异步返回
// exclude_dirs 是需要排除的文件夹，比如 .git 和 .github 文件夹 [".git", ".github"]
func file_processor(dir string, exclude_dirs []string, files_ch chan map[string]string) {
    files_map := make(map[string]string)

    err := filepath.Walk(dir, func(path string, f os.FileInfo, err error) error {
        if (f == nil) {return err}
        if f.IsDir() {
            fmt.Printf("isdir: %s\n", path)
            return nil
        }

        var target_dir_str string

        if (strings.Contains(path, "\\")) {
            path_tmp := strings.Split(path, "\\")
            target_dir_str = path_tmp[len(path_tmp) - 2]
        } else if (strings.Contains(path, "/")) {
            path_tmp := strings.Split(path, "/")
            target_dir_str = path_tmp[len(path_tmp) - 2]
        } else {
            fmt.Println("Error: not support path!")
            return nil
        }

        for v := range(exclude_dirs) {
            if (target_dir_str == exclude_dirs[v]) {
                fmt.Printf("exclude dir: %s\n", exclude_dirs[v])
                return nil
            }
        }

        // println(path)
        file_md5 := md5sum(path)

        r_path := []rune(path)
        new_path := string(r_path[len(dir) + 1:])
        files_map[new_path] = file_md5
        fmt.Printf("md5:%s : %s\n", file_md5, new_path)
        return nil
    })
    if err != nil {
        fmt.Printf("filepath.Walk() returned %v\n", err)
    }

    files_ch <- files_map
}

// 交集
// 返回值1：map_a 与 map_b 的交集
// 返回值2：map_a 相对 map_b 的差集，里面是 map_a 独有的数据
// 返回值3：map_b 独有的数据
func map_set_intersection(map_a map[string]string, map_b map[string]string) (
    intersection_map map[string]string,
    unique_map_a map[string]string, 
    unique_map_b map[string]string,
    changed_map map[string]string) {

    intersection_map = make(map[string]string)
    unique_map_a = make(map[string]string)
    unique_map_b = make(map[string]string)
    changed_map  = make(map[string]string)

    for key := range map_a {
        value, ok := map_b[key]
        if (ok) {
            intersection_map[key] = map_a[key]

            if (value != map_a[key]) {
                changed_map[key] = map_a[key]
            }
        } else {
            unique_map_a[key] = map_a[key]
        }
    }
    
    for key := range map_b {
        _, ok := map_a[key]
        if (!ok) {
            unique_map_b[key] = map_b[key]
        }
    }

    return intersection_map, unique_map_a, unique_map_b, changed_map
}

func cos_prepare(bucket_name string, secret_id string, secret_key string) *cos.Client {
    cos_url := fmt.Sprintf("https://%s.cos.ap-chengdu.myqcloud.com", bucket_name)
    fmt.Println(cos_url)

    url_no_path, _ := url.Parse(cos_url)
    base_url := &cos.BaseURL{BucketURL: url_no_path}

    cos_client := cos.NewClient(base_url, &http.Client{
        Transport: &cos.AuthorizationTransport{
            SecretID:  secret_id,
            SecretKey: secret_key,
            Transport: &debug.DebugRequestTransport{
                RequestHeader:  true,
                RequestBody:    false,
                ResponseHeader: true,
                ResponseBody:   true,
            },
        },
    })
    return cos_client
}

func cos_upload(c *cos.Client, file string, abs_path string) {
    var key string

    if (strings.Contains(file, "\\")) {
        key = strings.Join(strings.Split(file, "\\"), "/")
    } else {
        key = file
    }

    file_name := abs_path

    f, err := os.Open(file_name)
    if err != nil {
        panic(err)
    }
    defer f.Close()

    s, err := f.Stat()
    if err != nil {
        panic(err)
    }
    fmt.Println(s.Size())

    opt := &cos.ObjectPutOptions{
        ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{
            ContentLength: int(s.Size()),
        },
    }
    //opt.ContentLength = int(s.Size())

    _, err = c.Object.Put(context.Background(), key, f, opt)
    if err != nil {
        panic(err)
    }
}

func cos_delete(c *cos.Client, rm_files []string) {
    var key string

    if len(rm_files) == 0 {
        return
    }

    obs := []cos.Object{}
    for _, v := range rm_files {
        if (strings.Contains(v, "\\")) {
            key = strings.Join(strings.Split(v, "\\"), "/")
        } else {
            key = v
        }
    
        obs = append(obs, cos.Object{Key: key})
    }
    opt := &cos.ObjectDeleteMultiOptions{
        Objects: obs,
        // 布尔值，这个值决定了是否启动 Quiet 模式
        // 值为 true 启动 Quiet 模式，值为 false 则启动 Verbose 模式，默认值为 false
        // Quiet: true,
    }
    
    _, _, err := client.Object.DeleteMulti(context.Background(), opt)
    if err != nil {
        panic(err)
    }
}

func cos_main(new_files map[string]string, rm_files map[string]string, changed_files map[string]string) {
    client = cos_prepare(default_bucket_name, default_secretid, default_secretkey)

    for key := range new_files {
        fmt.Println(new_repo_dir + "\\" + key)
        cos_upload(client, key, new_repo_dir + "\\" + key)
    }

    var rm_file_list []string

    for key := range rm_files {
        rm_file_list = append(rm_file_list, key)
    }
    cos_delete(client, rm_file_list)
}

// ARGS: secretid secretkey region bucket_name old_dir new_dir
func main() {
    fmt.Printf("Tencent COS files sync client\n")

    if (len(os.Args) == 7) {
        default_secretid    = os.Args[1]
        default_secretkey   = os.Args[2]
        default_region      = os.Args[3]
        default_bucket_name = os.Args[4]
        old_repo_dir = os.Args[5]
        new_repo_dir = os.Args[6]
    } else {
        fmt.Printf("In args error\n")
        return
    }
    start_time := time.Now()

    var old_repo_ch = make(chan map[string]string)
    var new_repo_ch = make(chan map[string]string)

    var exclude_dirs []string = make([]string, 10)
    exclude_dirs[0] = ".git"
    exclude_dirs[1] = ".github"
    exclude_dirs[2] = ".gitee"

    go file_processor(old_repo_dir, exclude_dirs, old_repo_ch)
    go file_processor(new_repo_dir, exclude_dirs, new_repo_ch)
    
    old_repo_files := <-old_repo_ch
    new_repo_files := <-new_repo_ch
    fmt.Printf("old:\n%v\n", old_repo_files)
    fmt.Printf("new:\n%v\n", new_repo_files)

    common, unique_a, unique_b, changed := map_set_intersection(new_repo_files, old_repo_files)

    fmt.Printf("common files:\n%v\n", common)
    fmt.Printf("unique_new files:\n%v\n", unique_a)
    fmt.Printf("unique_old files:\n%v\n", unique_b)
    fmt.Printf("changed files:\n%v\n", changed)

    cos_main(unique_a, unique_b, changed)

    cost_time := time.Since(start_time)
    fmt.Printf("Run cost time: %s\n", cost_time)
}
