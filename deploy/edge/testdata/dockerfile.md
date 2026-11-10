# Synthetic dockerfile design corpus

```dockerfile
FROM scratch
# Synthetic design text, not a build instruction to execute.
COPY sample.txt /app/sample.txt
RUN printf example | sort && echo "$(date)"
ENTRYPOINT ["sh", "-c", "cat /app/sample.txt"]
```
