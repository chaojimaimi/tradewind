import Foundation

/// 网关数据面客户端（M2-plan §2.1）：HTTP/WS 请求构造，
/// 凭据显式 `Cookie: tradewind_panel=<token>` 头——不走 URLSession cookie storage，
/// 规避 WS 握手任务不带 storage 的坑。端口与 token 唯一来源：status --json 的 panel_url。
struct GatewayClient {
    static let cookieName = "tradewind_panel"
    /// 本机回环 Origin（网关 Host/Origin 白名单按 127.0.0.1:<port> 校验）
    let baseURL: URL
    let token: String
    /// 会话可注入（URLProtocol stub 单测；生产用 .shared）
    let session: URLSession

    /// 从 panel_url 解析：http://127.0.0.1:<port>/ui/<token>/ → (baseURL, token)。
    /// 形状不符（降级态空串等）返回 nil——调用方按"面板数据不可用"处理。
    init?(panelURL: String, session: URLSession = .shared) {
        guard let url = URL(string: panelURL),
              let host = url.host,
              let port = url.port,
              url.path.hasPrefix("/ui/")
        else { return nil }
        // path 形如 /ui/<64hex>/；token 段必须非空
        let segments = url.path.split(separator: "/").map(String.init)
        guard segments.count >= 2, segments[0] == "ui", !segments[1].isEmpty else { return nil }
        var components = URLComponents()
        components.scheme = url.scheme ?? "http"
        components.host = host
        components.port = port
        guard let base = components.url else { return nil }
        baseURL = base
        token = segments[1]
        self.session = session
    }

    /// 构造经网关的 GET 请求（纯函数，单测断言 Cookie 头注入）。
    /// WHY 不手动设 Host：CFNetwork 会按 URL 自行生成合法 Host（127.0.0.1:<port>）；
    /// 曾错误写入带 scheme 的 "Host: http://…"（非法值）→ 网关 400 → 数据面整体降级
    /// （M2 真机验收缺陷：curl 复现带该头 400 / 去掉 200）。
    func makeRequest(path: String) -> URLRequest {
        var request = URLRequest(url: baseURL.appendingPathComponent(path))
        request.setValue("\(Self.cookieName)=\(token)", forHTTPHeaderField: "Cookie")
        request.setValue(origin, forHTTPHeaderField: "Origin")
        return request
    }

    /// 白名单 Origin 形态：http://127.0.0.1:<port>（网关防伪造 Origin 校验的合法值）。
    var origin: String {
        var components = URLComponents()
        components.scheme = "http"
        components.host = baseURL.host
        components.port = baseURL.port
        return components.string ?? "http://127.0.0.1"
    }

    /// GET 一段网关 API（节点列表等低频拉取用；WS 速率流由 TrafficStore 负责）。
    func fetch(path: String) async throws -> Data {
        let (data, response) = try await session.data(for: makeRequest(path: path))
        guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) else {
            throw CoreBridgeError.cliFailed(command: "gateway \(path)")
        }
        return data
    }

    /// 构造带显式 Cookie 头的 WS 任务（/traffic 速率流；WHY URLRequest 形态：
    /// webSocketTask(with: URLRequest) 才能携带自定义头，URL 直构不带 storage）。
    func webSocketTask(path: String) -> URLSessionWebSocketTask {
        var request = URLRequest(url: baseURL.appendingPathComponent(path))
        request.setValue("\(Self.cookieName)=\(token)", forHTTPHeaderField: "Cookie")
        return session.webSocketTask(with: request)
    }
}

/// GET /proxies 的裁剪解码模型（demo 节点列表所需字段）。
struct GatewayProxies: Codable {
    struct Entry: Codable {
        let name: String
        let type: String
        let now: String?
        let all: [String]?
        let history: [History]?
        /// URLTest/Fallback 组的手动固定节点名（v0.4.16；固定态徽标+解除入口依据；
        /// mihomo 官方字段，未固定态线格式为空串——消费侧归一化，见 PanelModel）
        let fixed: String?
    }

    struct History: Codable {
        let delay: Int
    }

    let proxies: [String: Entry]
}

extension GatewayClient {
    /// 拉取全量代理并解码；解码失败显式报错（面板数据不可用降级路径）。
    func proxies() async throws -> GatewayProxies {
        let data = try await fetch(path: "proxies")
        do {
            return try JSONDecoder().decode(GatewayProxies.self, from: data)
        } catch {
            throw CoreBridgeError.decode("proxies: \(error.localizedDescription)")
        }
    }
}
