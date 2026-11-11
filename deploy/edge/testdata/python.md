# Synthetic python design corpus

```python
import subprocess
from pathlib import Path
result = subprocess.run(["sh", "-c", "printf example | sort"], capture_output=True)
pattern = r"^(?P<name>[a-z]+)\s*=\s*(.+)$"
query = "SELECT * FROM users WHERE id = %s"
```
