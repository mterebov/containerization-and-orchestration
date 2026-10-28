# Лабораторная работа 3 - Платформа для shop

## Часть 0 - подготовка окружения и подопытного сервиса

### Окружение

Перед началом работы, пока клод создавал сервис, было установлено необходимое окружение

``` bash
brew install kind helm
```

Создан кластер из `kind.yaml` конфига

``` yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: lab
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 30080
        hostPort: 30080
        protocol: TCP

```

`extraPortMappings` тут не просто так: порт 30080 мака пробрасывается на NodePort ноды. В части 4 мы будем гасить control plane, а `kubectl port-forward` ходит через apiserver и умрёт вместе с ним. NodePort обслуживает kube-proxy на самой ноде, так что через него можно будет проверить, что сервис жив и без apiserver.

``` bash
kind create cluster --config lab-3/kind.yaml
```

результат на скриншоте ниже

![p0-cluster](report/p0-cluster.png)

### Сервис

Сервис снова был создан клодом и снова на Go

- API:
    - **POST** `/order` Создает заказ
    - **GET** `/orders` выдает все заказы
    - **GET** `/version`
    - **GET** `/metrics`
    - **GET** `/health` (503, если `HEALTH_FAIL=true`, базу не трогает)

База через `DATABASE_URL`, без неё сервис стартует и живёт, это нужно, потому что Часть 2 идёт раньше Части 3

- Worker - раз в `POLL_INTERVAL` (5s) помечает пачку заказов `processed` через `FOR UPDATE SKIP LOCKED`, чтобы две реплики не хватали один заказ
 
    - `/health `
    - `/metrics` 
    - порт 8081

### Образы

Dockerfile у обоих сервисов одинаковый: multi-stage, сборка в `golang:1.27`, итоговый образ от `scratch` с одним бинарником и `USER 10001:10001`, версия зашивается через `--build-arg VERSION` и отдаётся в `/version`. Это пригодится в части 1 (правило про non-root) и в части 2 (видеть, какая версия отвечает во время rolling update).

Реестра у нас нет, образы грузятся в kind напрямую:

``` bash
docker build --build-arg VERSION=0.1.0 -t shop.local/api:0.1.0 lab-3/api
docker build --build-arg VERSION=0.1.0 -t shop.local/worker:0.1.0 lab-3/worker
kind load docker-image shop.local/api:0.1.0 shop.local/worker:0.1.0 --name lab
```

Префикс `shop.local/` это не реальный реестр: `kind load` кладёт образ в containerd ноды под полным именем, а `imagePullPolicy: IfNotPresent` находит его по имени и никуда не ходит. Зато в части 1 по этому префиксу правило про доверенный реестр будет отличать наши образы от чужих.

![p0-build-load](report/p0-build-load.png)

Проверка, что образы доехали до ноды:

``` bash
docker exec lab-control-plane crictl images | grep shop.local
```

![p0-crictl](report/p0-crictl.png)

## Часть 1 - что-то про политику и 5 кривых манифестов

### Выбор движка

Взяли Kyverno. Правила в нём это обычные Kubernetes-манифесты (`ClusterPolicy`), без отдельного языка, и на kind на ноутбуке он легче. Gatekeeper гибче, но учить Rego ради пяти правил смысла нет.

Ставится одним чартом, версию пинним, values свои и только с тем, что реально меняем: оставляем один admission controller, фоновое сканирование, отчёты и cleanup выключаем, в kind они просто едят память.

``` bash
helm upgrade --install kyverno kyverno/kyverno --version 3.9.2 -n kyverno --create-namespace -f lab-3/kyverno-values.yaml
kubectl -n kyverno get pods && kubectl get validatingwebhookconfigurations
```

![p1-kyverno](report/p1-kyverno.png)

Список вебхуков и есть механизм: Kyverno регистрирует в apiserver validating webhook, и apiserver на каждый запрос синхронно спрашивает его до записи в etcd. Отбитый манифест до etcd не доезжает, kubectl получает отказ от apiserver с текстом из `message` правила.

Kyverno 1.19 при apply предупреждает, что `ClusterPolicy` deprecated и надо переезжать на CEL-политики (`ValidatingPolicy`). Работает всё по-прежнему, для лабы оставили старый формат, он читается без знания CEL.

### Что исключили

Правила cluster-wide, но из-под них явно выведены инфраструктурные namespace: `kube-system`, `kyverno`, `monitoring`, `cnpg-system`. Стек мониторинга из лабы 2 и оператор базы ставятся из чужих чартов, у половины их подов нет limits, а образы из `quay.io` и `ghcr.io`. Переписывать чужие values ради правил, которые мы придумали для своих сервисов, неправильно, поэтому правила защищают рабочие namespace, а инфра живёт по своим чартам. `kube-system` и `kyverno` Kyverno и так не трогает через `resourceFilters`, но в политиках они перечислены явно, чтобы бандл читался сам по себе.

### Пять правил

Все лежат в `lab-3/policies/`, у всех одна структура: `match` на `Pod`, `exclude` инфры, `validate` с `failureAction: Enforce` (Audit только записал бы в отчёт) и `pattern`. Правило написано для `Pod`, но Kyverno через autogen сам распространяет его на `Deployment`, `StatefulSet`, `Job` и остальные контроллеры, поэтому кривой `helm install` упадёт сразу на Deployment, а не потом на подах.

- `require-limits`: у каждого контейнера есть `resources.limits.cpu` и `memory`. Без лимитов один под может съесть всю ноду, с лимитами cgroup его режет (как в лабе 1), а scheduler знает, что куда влезает.
- `restrict-registry`: образы только `shop.local/*`. Образ из неизвестного реестра это чужой код в кластере. Правило закрывает все три списка контейнеров: `containers`, `initContainers` и `ephemeralContainers`, иначе обходится через init-контейнер или `kubectl debug`. В части 3 сюда добавится реестр оператора базы.
- `require-labels`: у пода обязателен `app.kubernetes.io/name`. По лейблам работают селекторы сервисов, `ServiceMonitor` и дашборды, под без лейбла для всего этого невидим.
- `disallow-privileged`: `securityContext.privileged` не может быть `true`. Privileged-контейнер это root на ноде со всеми устройствами, вся изоляция из лабы 1 выключается одним полем.
- `require-nonroot`, наше пятое: у пода обязателен `securityContext.runAsNonRoot: true`. Kubelet тогда перед стартом проверяет, под кем собирается работать образ, и не пускает uid 0. Поэтому в Dockerfile стоит именно числовой `USER 10001:10001`, по имени пользователя kubelet проверить не может.

``` bash
kubectl apply -f lab-3/policies/ && kubectl get clusterpolicy
```

![p1-policies](report/p1-policies.png)

### Пять кривых подов

В `lab-3/policies/bad/` один правильный под, который проходит все пять правил, и пять копий, в каждой сломано ровно одно: `broken-limits` без `resources`, `broken-registry` с `nginx:1.27`, `broken-labels` без `labels`, `broken-privileged` с `privileged: true` у контейнера, `broken-nonroot` с `runAsNonRoot: false`. Ломать по одному важно, иначе непонятно, какое правило сработало.

``` bash
kubectl create ns shop
for f in lab-3/policies/bad/*.yaml; do echo "== $f"; kubectl apply -n shop -f "$f"; done
kubectl -n shop get pods
```

![p1-rejected](report/p1-rejected.png)

Все пять отбиты с текстом из `message`, в `shop` пусто: поды не создаются вообще, а не удаляются потом.

Попутно два манифеста с первого раза упали не на Kyverno: в одном имя пода было с заглавными буквами, в другом `privileged` лежал в `securityContext` пода, а такого поля там нет. Первый отбила встроенная валидация apiserver, второй не прошёл декодирование по схеме. Порядок в apiserver такой: декодирование → встроенная валидация → admission-вебхуки, и политики видят только то, что прошло первые два шага.

## Часть 2 - потрогать Helm-чарт 

## Часть 3 - Слон в ящике

## Часть 4 - Chaos Engineering эксперимент

## Часть 5 - Мониторинг
