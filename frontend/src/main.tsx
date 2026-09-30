import React from "react";
import ReactDOM from "react-dom/client";
import { App } from "./App";
import "./styles.css";

// Render failures must surface as a readable message instead of a blank
// white page.
class ErrorBoundary extends React.Component<React.PropsWithChildren, { error?: Error }> {
  state: { error?: Error } = {};
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  render() {
    if (this.state.error) {
      return (
        <div style={{ padding: 48, textAlign: "center" }}>
          <h2>页面渲染出错</h2>
          <p style={{ color: "#8b97ad" }}>{String(this.state.error.message || this.state.error)}</p>
          <p style={{ color: "#8b97ad" }}>请刷新重试；演练状态已持久化，不会丢失。</p>
        </div>
      );
    }
    return this.props.children;
  }
}

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ErrorBoundary>
      <App />
    </ErrorBoundary>
  </React.StrictMode>
);
