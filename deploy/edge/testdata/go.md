# Synthetic go design corpus

```go
package design
import "os/exec"
func Render(query string) { cmd := exec.Command("sh", "-c", "printf sample | sort"); _ = cmd; _ = query }
// Data only: db.Query("SELECT * FROM users WHERE id = ?", id)
```
