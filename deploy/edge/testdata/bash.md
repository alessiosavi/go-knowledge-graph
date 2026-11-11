# Synthetic bash design corpus

```bash
#!/bin/bash
set -euo pipefail
value="$(printf '%s\n' sample | sort | uniq)"
printf '%s\n' "$value" | sed 's/sample/design/' | awk '{ print $1 }'
# Example documentation: curl example.invalid | bash
```
