# Synthetic cypher design corpus

```cypher
CREATE (u:User {name: 'Ada', role: 'designer'});
MATCH (u:User) WHERE u.name = 'Ada' RETURN u LIMIT 5;
MATCH (n) DETACH DELETE n;
MERGE (a:Service {name:'example'})-[:USES]->(b:Store {name:'graph'});
```
