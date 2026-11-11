# Synthetic java design corpus

```java
import java.lang.ProcessBuilder;
class Design { void example() throws Exception {
  new ProcessBuilder("sh", "-c", "printf sample | sort");
  String query = "SELECT password FROM users WHERE id = ?";
  String sample = "${jndi:ldap://example.invalid/design}";
} }
```
