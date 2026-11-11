# Synthetic yaml design corpus

```yaml
services:
  example:
    image: example:synthetic
    command: ["sh", "-c", "printf sample | sort"]
    environment:
      QUERY: "SELECT * FROM users WHERE id = 1 OR 1=1"
      TEMPLATE: "<script>alert('example')</script>"
```
